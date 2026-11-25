// Package imagevolume materializes Kubernetes image volumes (VolumeSource.Image) for macOS VMs.
//
// The guest cannot pull: the host pulls the OCI image, applies its layers in order and shares the
// resulting tree read-only into the guest over VirtioFS, as a hostPath is shared. Each layer chain
// (the image's first n layers) is one directory under <cache>/image-volumes/chains, named by the
// chain's digest: an APFS clone (clonefile) of the chain below plus that layer. Images that share
// lower layers therefore share their trees, and a new top layer costs only its own bytes. A chain
// directory appears by rename only once complete, so a crash or a cancelled pull leaves no partial
// tree and the next request resumes at the first missing chain.
package imagevolume

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"golang.org/x/sync/singleflight"
	"golang.org/x/sys/unix"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/agoda-com/macOS-vz-kubelet/internal/diskguard"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
)

// BuildTimeout bounds one chain's pull and apply, which runs detached from the requesting pod so
// that a pod deleted mid-pull (an expired Job deadline) still leaves the chain cached for the next.
const BuildTimeout = 15 * time.Minute

// expansion is the assumed ratio of a layer's unpacked size to its compressed size, for the
// free-space check before a layer is applied.
const expansion = 4

const (
	mediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerLayer        = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

// Store holds the materialized chains under Root.
type Store struct {
	Root         string // <cache>/image-volumes
	MinFreeBytes uint64 // free-space floor on the cache volume (VZ_MIN_FREE_BYTES)

	// Fetcher reaches the registry; nil uses oras remote repositories.
	Fetcher func(ref string, creds resource.RegistryCredentials) (Fetcher, error)

	flight singleflight.Group
}

// Fetcher is the registry surface the store uses (oras remote.Repository satisfies it).
type Fetcher interface {
	FetchReference(ctx context.Context, reference string) (ocispec.Descriptor, io.ReadCloser, error)
	Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error)
}

// New returns a store rooted at <cachePath>/image-volumes.
func New(cachePath string, minFreeBytes uint64) *Store {
	return &Store{Root: filepath.Join(cachePath, "image-volumes"), MinFreeBytes: minFreeBytes}
}

// Materialize returns the directory holding the applied tree of image ref, pulling and applying
// whatever chains are missing. The directory must not be written: it is shared read-only.
func (s *Store) Materialize(ctx context.Context, ref string, creds resource.RegistryCredentials) (string, error) {
	repo, err := s.fetcher(ref, creds)
	if err != nil {
		return "", err
	}
	layers, err := resolveLayers(ctx, repo, referenceOf(ref))
	if err != nil {
		return "", fmt.Errorf("image volume %s: %w", ref, err)
	}
	if len(layers) == 0 {
		return "", fmt.Errorf("image volume %s: the image has no layers", ref)
	}
	parent := ""
	for i, layer := range layers {
		chain := ChainID(parent, layer.Digest)
		dir := s.chainDir(chain)
		if _, err := os.Lstat(dir); err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			if err := s.build(ctx, repo, ref, i, layer, parent, chain); err != nil {
				return "", fmt.Errorf("image volume %s: layer %d (%s): %w", ref, i, layer.Digest, err)
			}
		}
		parent = chain
	}
	dir := s.chainDir(parent)
	// The chain's mtime records its last use, for whoever later prunes unused chains.
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
	return dir, nil
}

// ChainID names the tree of a parent chain plus one layer ("" is the empty parent).
func ChainID(parent string, layer digest.Digest) string {
	sum := sha256.Sum256([]byte(parent + "\n" + layer.String()))
	return hex.EncodeToString(sum[:])
}

func (s *Store) chainDir(chain string) string { return filepath.Join(s.Root, "chains", chain) }

func (s *Store) fetcher(ref string, creds resource.RegistryCredentials) (Fetcher, error) {
	if s.Fetcher != nil {
		return s.Fetcher(ref, creds)
	}
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("image volume reference %s: %w", ref, err)
	}
	if !creds.IsEmpty() {
		repo.Client = &auth.Client{Credential: auth.StaticCredential(repo.Reference.Registry, auth.Credential{
			Username: creds.Username, Password: creds.Password, RefreshToken: creds.IdentityToken,
		})}
	}
	return repo, nil
}

func referenceOf(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return "latest"
}

// build applies one layer over its parent chain, once per chain however many pods ask, detached
// from the asking pod's context; the caller waits for it or for its own context.
func (s *Store) build(ctx context.Context, repo Fetcher, ref string, index int, layer ocispec.Descriptor, parent, chain string) error {
	ch := s.flight.DoChan(chain, func() (any, error) {
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), BuildTimeout)
		defer cancel()
		return nil, s.apply(bctx, repo, ref, index, layer, parent, chain)
	})
	select {
	case r := <-ch:
		return r.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) apply(ctx context.Context, repo Fetcher, ref string, index int, layer ocispec.Descriptor, parent, chain string) (err error) {
	dir := s.chainDir(chain)
	if _, err := os.Lstat(dir); err == nil {
		return nil // another request finished it
	}
	tmpRoot := filepath.Join(s.Root, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if free, ferr := diskguard.FreeBytes(tmpRoot); ferr == nil && layer.Size > 0 {
		if needed := uint64(layer.Size) * expansion; !diskguard.Fits(free, needed, s.MinFreeBytes) {
			return fmt.Errorf("applying needs about %d bytes, %d free, floor %d", needed, free, s.MinFreeBytes)
		}
	}
	work := filepath.Join(tmpRoot, fmt.Sprintf("%s-%d", chain, time.Now().UnixNano()))
	defer func() {
		if err != nil {
			_ = os.RemoveAll(work)
		}
	}()
	if parent == "" {
		if err := os.Mkdir(work, 0o755); err != nil {
			return err
		}
	} else if err := unix.Clonefile(s.chainDir(parent), work, unix.CLONE_NOFOLLOW); err != nil {
		return fmt.Errorf("clone parent chain %s: %w", parent, err)
	}
	started := time.Now()
	rc, err := repo.Fetch(ctx, layer)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer rc.Close()
	verified := content.NewVerifyReader(rc, layer)
	stream, closeStream, err := decompress(layer.MediaType, verified)
	if err != nil {
		return err
	}
	defer closeStream()
	if err := Apply(work, stream); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, stream); err != nil { // trailing padding, so the digest covers the whole blob
		return err
	}
	if err := verified.Verify(); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if err := os.Rename(work, dir); err != nil {
		if _, statErr := os.Lstat(dir); statErr == nil {
			_ = os.RemoveAll(work)
			return nil
		}
		return err
	}
	log.G(ctx).Infof("Image volume %s: applied layer %d (%s, %d bytes) as chain %s in %s", ref, index, layer.Digest, layer.Size, chain, time.Since(started).Round(time.Millisecond))
	return nil
}

func decompress(mediaType string, r io.Reader) (io.Reader, func(), error) {
	switch {
	case mediaType == ocispec.MediaTypeImageLayer:
		return r, func() {}, nil
	case mediaType == ocispec.MediaTypeImageLayerGzip || mediaType == mediaTypeDockerLayer:
		z, err := pgzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip: %w", err)
		}
		return z, func() { _ = z.Close() }, nil
	case mediaType == ocispec.MediaTypeImageLayerZstd:
		z, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd: %w", err)
		}
		return z, z.Close, nil
	}
	return nil, nil, fmt.Errorf("layer media type %q is not a tar layer (tar, tar+gzip or tar+zstd)", mediaType)
}

// resolveLayers reads the image manifest of reference; an index resolves to its first image
// manifest that is not an attestation (the tree is the same bytes whatever the build platform).
func resolveLayers(ctx context.Context, repo Fetcher, reference string) ([]ocispec.Descriptor, error) {
	desc, rc, err := repo.FetchReference(ctx, reference)
	if err != nil {
		return nil, err
	}
	for depth := 0; ; depth++ {
		body, err := content.ReadAll(rc, desc)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		switch desc.MediaType {
		case ocispec.MediaTypeImageManifest, mediaTypeDockerManifest:
			var m ocispec.Manifest
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, err
			}
			return m.Layers, nil
		case ocispec.MediaTypeImageIndex, mediaTypeDockerManifestList:
			if depth > 0 {
				return nil, errors.New("nested image index")
			}
			var idx ocispec.Index
			if err := json.Unmarshal(body, &idx); err != nil {
				return nil, err
			}
			next, ok := imageManifest(idx)
			if !ok {
				return nil, errors.New("the image index lists no image manifest")
			}
			desc = next
			if rc, err = repo.Fetch(ctx, desc); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported manifest media type %q", desc.MediaType)
		}
	}
}

func imageManifest(idx ocispec.Index) (ocispec.Descriptor, bool) {
	for _, m := range idx.Manifests {
		if m.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
			continue
		}
		if m.Platform != nil && m.Platform.OS == "unknown" {
			continue
		}
		return m, true
	}
	return ocispec.Descriptor{}, false
}

// Apply extracts one tar layer over root with OCI layer semantics (whiteouts remove, a later entry
// replaces an earlier one). No entry is written outside root: every parent component of an entry
// must be a real directory, never a symlink, so a symlink in the tree cannot redirect a write.
// Device nodes and FIFOs are skipped; set-id bits are dropped; owners are the provider's user.
func Apply(root string, r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read layer: %w", err)
		}
		rel, err := entryPath(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue // the root itself
		}
		dirRel, base := path.Split(rel)
		parent, err := ensureDirs(root, strings.TrimSuffix(dirRel, "/"))
		if err != nil {
			return err
		}
		if strings.HasPrefix(base, ".wh.") {
			if base == ".wh..wh..opq" {
				if err := emptyDir(parent); err != nil {
					return err
				}
			} else if err := os.RemoveAll(filepath.Join(parent, strings.TrimPrefix(base, ".wh."))); err != nil {
				return err
			}
			continue
		}
		target := filepath.Join(parent, base)
		mode := os.FileMode(hdr.Mode) & os.ModePerm
		switch hdr.Typeflag {
		case tar.TypeDir:
			if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
				if err := os.RemoveAll(target); err != nil {
					return err
				}
			}
			if err := os.Mkdir(target, mode|0o700); err != nil && !os.IsExist(err) {
				return err
			}
			if err := os.Chmod(target, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := replace(target); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o200)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("write %s: %w", rel, err)
			}
		case tar.TypeSymlink:
			if err := replace(target); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			linkRel, err := entryPath(hdr.Linkname)
			if err != nil || linkRel == "" {
				return fmt.Errorf("hard link %s -> %q: %w", rel, hdr.Linkname, errors.Join(err, errors.New("target outside the layer")))
			}
			linkDir, linkBase := path.Split(linkRel)
			linkParent, err := ensureDirs(root, strings.TrimSuffix(linkDir, "/"))
			if err != nil {
				return err
			}
			source := filepath.Join(linkParent, linkBase)
			if fi, err := os.Lstat(source); err != nil || !fi.Mode().IsRegular() {
				return fmt.Errorf("hard link %s -> %s: the target is not a regular file of the tree", rel, linkRel)
			}
			if err := replace(target); err != nil {
				return err
			}
			if err := os.Link(source, target); err != nil {
				return err
			}
		default:
			// character and block devices, FIFOs: nothing a shared tree needs
		}
	}
}

// entryPath returns name as a clean path relative to the layer root: cleaning it as an absolute
// path drops every leading "..", so it never names anything above the root.
func entryPath(name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("layer entry %q has a NUL byte", name)
	}
	return strings.TrimPrefix(path.Clean("/"+name), "/"), nil
}

// ensureDirs returns root/rel, creating each missing directory and refusing any component that
// exists as something other than a real directory.
func ensureDirs(root, rel string) (string, error) {
	cur := root
	if rel == "" {
		return cur, nil
	}
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case os.IsNotExist(err):
			if err := os.Mkdir(cur, 0o755); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case fi.Mode()&os.ModeSymlink != 0:
			return "", fmt.Errorf("layer writes through %s, a symlink", cur)
		case !fi.IsDir():
			return "", fmt.Errorf("layer writes under %s, not a directory", cur)
		}
	}
	return cur, nil
}

func replace(target string) error {
	if _, err := os.Lstat(target); err == nil {
		return os.RemoveAll(target)
	}
	return nil
}

func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
