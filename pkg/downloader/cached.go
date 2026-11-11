package downloader

import (
	"context"
	"fmt"
	"os"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageDir returns the cache directory of ref under storePath, refusing a reference that would
// map outside <storePath>/blobs.
func ImageDir(storePath, ref string) (string, error) {
	return oci.ImageDir(storePath, ref)
}

// CachedImage is an image whose every layer is in the cache and digest-verified.
type CachedImage struct {
	Dir            string
	ManifestDigest string
	Layers         []ocispec.Descriptor
}

// VerifyCached uses a local import manifest when present, otherwise resolves the registry
// manifest (no layers). It
// requires every layer of it to be already cached and validated (isCached: the digest
// sidecar is newer than the file and records the manifest's digest). It never pulls.
func VerifyCached(ctx context.Context, storePath, ref string, creds resource.RegistryCredentials) (CachedImage, error) {
	dir, err := ImageDir(storePath, ref)
	if err != nil {
		return CachedImage{}, err
	}
	m, d, err := oci.ReadLocalManifest(ctx, dir)
	var layers []ocispec.Descriptor
	manifestDigest := d.String()
	if os.IsNotExist(err) && !oci.HasLocalManifest(dir) {
		desc, remoteLayers, remoteErr := fetchLayers(ctx, ref, creds)
		err, layers, manifestDigest = remoteErr, remoteLayers, desc.Digest.String()
	} else {
		layers = m.Layers
	}
	if err != nil {
		return CachedImage{}, err
	}
	for _, l := range layers {
		if !isCached(l, dir) {
			return CachedImage{}, fmt.Errorf("image %s is not fully cached and verified (layer %q); import or pull it again", ref, l.Annotations[ocispec.AnnotationTitle])
		}
	}
	return CachedImage{Dir: dir, ManifestDigest: manifestDigest, Layers: layers}, nil
}

// FreeBytes reports the space available on the volume holding path.
func FreeBytes(path string) (uint64, error) { return freeBytes(path) }

// Fits reports whether free space after writing needed bytes stays at or above floor.
func Fits(free, needed, floor uint64) bool { return fits(free, needed, floor) }

// WatchFreeSpace returns a context that is cancelled with ErrInsufficientSpace as its cause if
// free space on path drops below floor (0 disables). Call stop when done.
func WatchFreeSpace(ctx context.Context, path string, floor uint64) (context.Context, func()) {
	return watchFreeSpace(ctx, path, floor)
}
