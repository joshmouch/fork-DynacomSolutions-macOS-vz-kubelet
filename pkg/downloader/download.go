package downloader

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/vm/config"
	"github.com/virtual-kubelet/virtual-kubelet/trace"

	"k8s.io/apimachinery/pkg/util/wait"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

const (
	DefaultMinRetryDelay = 2 * time.Second  // Default minimum delay between retries.
	DefaultMaxDelay      = 60 * time.Second // Default maximum delay between retries.
	DefaultMaxAttempts   = 5                // Default maximum number of retry attempts.
	DefaultFactor        = 1.6              // Default factor to increase the delay between retries.
	DefaultJitter        = 0.2              // Default jitter to add to delays.
)

// Params contains the parameters for downloading an OCI image.
type Params struct {
	Ref             string
	StorePath       string
	IgnoreExisiting bool
	Credentials     resource.RegistryCredentials

	// MinFreeBytes is the free-space floor on the cache volume; 0 disables the guard.
	MinFreeBytes uint64
	// InUse returns the cached image directories that must never be evicted; called
	// under the guard lock so it is current at decision time.
	InUse func() []string
	// VMUse returns the cached image directories of every VM on the node, one entry per VM.
	// An image shared by more than one VM keeps its files on a re-pull: another VM may have
	// pulled it and not yet opened the disk.
	VMUse func() []string

	MinRetryDelay time.Duration
	MaxDelay      time.Duration
	MaxAttempts   int
}

// Download downloads an OCI image and returns a Config.
// It uses ORAS to pull the image and store it within the local store.
// It retries the download operation with exponential backoff.
func Download(ctx context.Context, params Params, eventRecorder event.EventRecorder) (cfg config.MacPlatformConfigurationOptions, err error) {
	ctx, span := trace.StartSpan(ctx, "Downloader.Download")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if params.MinRetryDelay == 0 {
		params.MinRetryDelay = DefaultMinRetryDelay
	}
	if params.MaxDelay == 0 {
		params.MaxDelay = DefaultMaxDelay
	}
	if params.MaxAttempts == 0 {
		params.MaxAttempts = DefaultMaxAttempts
	}

	imageDir, err := ImageDir(params.StorePath, params.Ref)
	if err != nil {
		return cfg, err
	}
	_, statErr := os.Stat(imageDir)
	createdDir := statErr != nil // a failed pull must only remove a directory it created

	store, err := oci.New(imageDir, params.IgnoreExisiting, eventRecorder)
	if err != nil {
		return cfg, fmt.Errorf("failed to initialize store: %w", err)
	}
	defer func() {
		err = errors.Join(err, store.Close(ctx))
	}()

	// Local imports deliberately have no registry copy. Never resolve their tags
	// remotely for IfNotPresent, and never hide an incomplete local cache by pulling.
	if !params.IgnoreExisiting {
		m, _, localErr := oci.ReadLocalManifest(ctx, imageDir)
		if localErr == nil {
			for _, layer := range m.Layers {
				if !isCached(layer, imageDir) {
					return cfg, fmt.Errorf("image %s is not fully cached and verified", params.Ref)
				}
				if ok, err := store.Exists(ctx, layer); err != nil || !ok {
					return cfg, fmt.Errorf("local layer %s is unavailable: %v", layer.Digest, err)
				}
			}
			now := time.Now()
			_ = os.Chtimes(imageDir, now, now)
			return platformOptions(ctx, store)
		}
		if !os.IsNotExist(localErr) {
			return cfg, localErr
		}
	}

	var spaceErr error
	err = wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
		Duration: params.MinRetryDelay, // Base delay to start with
		Factor:   DefaultFactor,        // Factor to increase the delay between retries
		Jitter:   DefaultJitter,        // Randomization factor to avoid thundering herd problem
		Steps:    params.MaxAttempts,   // Maximum number of retry attempts
		Cap:      params.MaxDelay,      // Maximum delay between retries
	}, func(ctx context.Context) (done bool, _ error) { // never use condition error
		err = guardedPull(ctx, params, imageDir, store)
		if errors.Is(err, ErrInsufficientSpace) {
			spaceErr = err // permanent: do not retry
			return true, nil
		}
		if err != nil {
			// log error, but do not return it to continue retrying
			eventRecorder.FailedToPullImage(ctx, params.Ref, "", err)
		}
		return err == nil, nil
	})

	if spaceErr != nil {
		if createdDir {
			_ = os.RemoveAll(imageDir)
		}
		return cfg, spaceErr
	}
	if err != nil {
		return cfg, err
	}
	// An explicit Always pull replaces any previous local-only reference.
	if params.IgnoreExisiting {
		for _, name := range []string{oci.LocalManifestName, oci.LocalManifestName + ".digest"} {
			if err := os.Remove(filepath.Join(imageDir, name)); err != nil && !os.IsNotExist(err) {
				return cfg, err
			}
		}
	}
	// Mark the image as used now, so eviction order is least recently used first.
	now := time.Now()
	_ = os.Chtimes(imageDir, now, now)

	return platformOptions(ctx, store)
}

// platformOptions reads the pulled files from store. A disk-only image (a data disk: no aux
// layer and no config layer) returns just BlockStoragePath; an image missing only one of the
// aux and config layers is incomplete and fails.
func platformOptions(ctx context.Context, store *oci.Store) (cfg config.MacPlatformConfigurationOptions, err error) {
	blkStoragePath, err := store.GetFilePathForMediaType(ctx, oci.MediaTypeDiskImage)
	if err != nil {
		return cfg, fmt.Errorf("failed to get file path for media type %s: %w", oci.MediaTypeDiskImage, err)
	}
	if !store.HasMediaType(oci.MediaTypeAuxImage) && !store.HasMediaType(oci.MediaTypeConfigV1) {
		return config.MacPlatformConfigurationOptions{BlockStoragePath: blkStoragePath}, nil
	}
	auxStoragePath, err := store.GetFilePathForMediaType(ctx, oci.MediaTypeAuxImage)
	if err != nil {
		return cfg, fmt.Errorf("failed to get file path for media type %s: %w", oci.MediaTypeAuxImage, err)
	}
	c, err := store.GetConfig(ctx)
	if err != nil {
		return cfg, fmt.Errorf("failed to get config: %w", err)
	}
	return config.MacPlatformConfigurationOptions{
		BlockStoragePath:      blkStoragePath,
		AuxiliaryStoragePath:  auxStoragePath,
		HardwareModelData:     c.HardwareModelData,
		MachineIdentifierData: c.MachineIdData,
	}, nil
}

// ValidateImageReference reports whether ref parses as a pullable OCI reference.
// It uses the same parser pull uses (remote.NewRepository) so admission-time
// validation cannot drift from the actual pull.
func ValidateImageReference(ref string) error {
	_, err := remote.NewRepository(ref)
	return err
}

// guardedPull checks free space (evicting unused images if needed), then pulls
// while watching that free space stays above the floor.
func guardedPull(ctx context.Context, params Params, imageDir string, store *oci.Store) error {
	if params.MinFreeBytes == 0 {
		return pull(ctx, params.Ref, store, params.Credentials)
	}
	unlock, err := lockImage(ctx, imageDir)
	if err != nil {
		return err
	}
	defer unlock()
	release, pin, err := ensureSpace(ctx, params, imageDir)
	if err != nil {
		return err
	}
	defer release()
	pullCtx, stop := watchFreeSpace(ctx, params.StorePath, params.MinFreeBytes)
	defer stop()
	err = pullPinned(pullCtx, params.Ref, pin, store, params.Credentials)
	if cause := context.Cause(pullCtx); err != nil && errors.Is(cause, ErrInsufficientSpace) {
		return cause
	}
	return err
}

// newRepository builds the remote repository for ref with the given credentials.
func newRepository(ref string, creds resource.RegistryCredentials) (*remote.Repository, error) {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("failed to create repository from reference %s: %w", ref, err)
	}
	// Determine if the repository is using plain HTTP based on if it's localhost or a local IP
	repo.PlainHTTP = isLocalhostOrLocalIP(repo.Reference.Registry)

	if !creds.IsEmpty() {
		repo.Client = &auth.Client{
			Credential: auth.StaticCredential(repo.Reference.Registry, toORASCredential(creds)),
		}
	}
	return repo, nil
}

// pull pulls an OCI image from a remote repository and stores it in the local store.
func pull(ctx context.Context, ref string, store *oci.Store, creds resource.RegistryCredentials) error {
	return pullPinned(ctx, ref, "", store, creds)
}

// pullPinned is pull from the manifest digest pin when set (the one the disk guard
// estimated), so a retagged image cannot swap in a different size.
func pullPinned(ctx context.Context, ref, pin string, store *oci.Store, creds resource.RegistryCredentials) (err error) {
	ctx, span := trace.StartSpan(ctx, "OCI.pull")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	repo, err := newRepository(ref, creds)
	if err != nil {
		return err
	}

	ctx = auth.AppendRepositoryScope(ctx, repo.Reference, auth.ActionPull)
	src := repo.Reference.Reference
	if pin != "" {
		src = pin
	}
	_, err = oras.Copy(ctx, repo, src, store, repo.Reference.Reference, oras.DefaultCopyOptions)
	if err != nil {
		return err
	}

	return nil
}

func toORASCredential(creds resource.RegistryCredentials) auth.Credential {
	// IdentityToken rides RefreshToken: oras-go exchanges it via the registry OAuth2
	// grant_type=refresh_token flow. Username/Password still pass through for Basic and
	// distribution-token auth.
	return auth.Credential{
		Username:     creds.Username,
		Password:     creds.Password,
		RefreshToken: creds.IdentityToken,
	}
}

// isLocalhostOrLocalIP returns true if the host is localhost or a local IP address.
func isLocalhostOrLocalIP(host string) bool {
	host = strings.Split(host, ":")[0] // stripping port if present
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
