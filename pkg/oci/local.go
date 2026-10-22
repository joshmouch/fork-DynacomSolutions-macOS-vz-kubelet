package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/disk"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
)

// LocalManifestName marks a deliberately local reference. Ordinary pulls do not
// persist manifests, so this marker also prevents accidental registry fallback.
const LocalManifestName = ".local-manifest.json"

// ImageDir is shared by the importer and downloader to keep reference paths identical.
//
// A digest reference (repo@sha256:...) with no directory of its own resolves to the local-only
// tag of the same repository whose manifest has that digest, so a Pod can pin an image that was
// built or imported on this node (build-image --local, import-tart --local) by digest.
func ImageDir(storePath, ref string) (string, error) {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return "", err
	}
	root := filepath.Join(storePath, "blobs")
	dir := filepath.Join(root, strings.ReplaceAll(ref, ":", "/"))
	if rel, err := filepath.Rel(root, dir); err != nil || !filepath.IsLocal(rel) || rel == "." {
		return "", fmt.Errorf("image reference %q maps outside the cache", ref)
	}
	if repo.Reference.ValidateReferenceAsDigest() != nil {
		return dir, nil
	}
	if _, err := os.Lstat(dir); err == nil {
		return dir, nil
	}
	repoDir := filepath.Join(root, strings.ReplaceAll(repo.Reference.Registry+"/"+repo.Reference.Repository, ":", "/"))
	tags, _ := os.ReadDir(repoDir)
	for _, tag := range tags {
		tagDir := filepath.Join(repoDir, tag.Name())
		if _, d, err := ReadLocalManifest(context.Background(), tagDir); tag.IsDir() && err == nil && d.String() == repo.Reference.Reference {
			return tagDir, nil
		}
	}
	return dir, nil
}

// ReadLocalManifest validates the local metadata. Only a missing marker permits
// callers to fall back to a registry; damaged metadata must fail closed.
func ReadLocalManifest(ctx context.Context, dir string) (ocispec.Manifest, digest.Digest, error) {
	var m ocispec.Manifest
	path := filepath.Join(dir, LocalManifestName)
	b, err := os.ReadFile(path)
	if err != nil {
		return m, "", err
	}
	d := digest.FromBytes(b)
	stored, err := os.ReadFile(path + disk.DigestFileSuffix)
	if err != nil || string(stored) != d.String() {
		return m, "", fmt.Errorf("local manifest digest is missing or invalid")
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, "", err
	}
	if m.SchemaVersion != 2 || m.MediaType != ocispec.MediaTypeImageManifest || len(m.Layers) == 0 {
		return m, "", errors.New("invalid local image manifest")
	}
	seen := make(map[string]bool)
	for _, l := range m.Layers {
		name := l.Annotations[ocispec.AnnotationTitle]
		if err := ValidateTitle(l); err != nil {
			return m, "", err
		}
		if name == "" || seen[name] || l.Digest.Validate() != nil || l.Size < 0 {
			return m, "", errors.New("invalid or duplicate local layer")
		}
		seen[name] = true
		if d := l.Annotations[AnnotationUncompressedDigest]; d != "" {
			if digest.Digest(d).Validate() != nil {
				return m, "", errors.New("invalid local uncompressed digest")
			}
			if _, err := ParseUncompressedSize(l.Annotations[AnnotationUncompressedSize]); err != nil {
				return m, "", err
			}
		}
	}
	if !seen[MediaTypeDiskImage.Title()] || seen[MediaTypeAuxImage.Title()] != seen[MediaTypeConfigV1.Title()] {
		return m, "", errors.New("incomplete local image manifest")
	}
	return m, d, ctx.Err()
}

// InstallLocal consumes private, immutable build inputs on the same volume. The
// completed directory is renamed into place only after every file is verified.
// Existing references are refused, so an active provider cannot observe replacement
// files under an old manifest and a failed import cannot damage a previous cache.
func InstallLocal(ctx context.Context, cacheDir, ref string, manifest []byte, paths map[string]string) (err error) {
	dir, err := ImageDir(cacheDir, ref)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		return fmt.Errorf("local destination already exists or cannot be inspected: %s", dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dir), ".import-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	meta := filepath.Join(stage, LocalManifestName)
	if err := os.WriteFile(meta, manifest, 0o600); err != nil {
		return err
	}
	if err := disk.WriteDigestFile(meta, digest.FromBytes(manifest)); err != nil {
		return err
	}
	m, _, err := ReadLocalManifest(ctx, stage)
	if err != nil {
		return err
	}
	for _, l := range m.Layers {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := l.Annotations[ocispec.AnnotationTitle]
		src := paths[name]
		d := l.Digest
		if raw := l.Annotations[AnnotationUncompressedDigest]; raw != "" {
			d = digest.Digest(raw)
		}
		// Reuse the puller's digest writer and verifier, including its sidecar format.
		if err := disk.ComputeAndVerifyFileDigest(src, d); err != nil {
			return fmt.Errorf("local layer %s: %w", name, err)
		}
		dst := filepath.Join(stage, name)
		if err := os.Rename(src, dst); err != nil {
			return err
		}
		if err := os.Rename(src+disk.DigestFileSuffix, dst+disk.DigestFileSuffix); err != nil {
			return err
		}
		// Some filesystems give consecutive writes the same timestamp. The provider
		// requires the marker to be strictly newer than the verified file.
		fi, err := os.Stat(dst)
		if err != nil {
			return err
		}
		stamp := time.Now()
		if !stamp.After(fi.ModTime()) {
			stamp = fi.ModTime().Add(time.Nanosecond)
		}
		if err := os.Chtimes(dst+disk.DigestFileSuffix, stamp, stamp); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(stage, dir)
}

// IsCached applies the provider's verified-sidecar check without registry access.
func IsCached(l ocispec.Descriptor, dir string) bool {
	name := l.Annotations[ocispec.AnnotationTitle]
	if name == "" {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	sidecar := filepath.Join(dir, name+disk.DigestFileSuffix)
	si, err := os.Stat(sidecar)
	if err != nil || !si.ModTime().After(fi.ModTime()) {
		return false
	}
	got, err := os.ReadFile(sidecar)
	return err == nil && string(got) == localLayerDigest(l).String()
}

func localLayerDigest(l ocispec.Descriptor) digest.Digest {
	if d := l.Annotations[AnnotationUncompressedDigest]; d != "" {
		return digest.Digest(d)
	}
	return l.Digest
}

// HasLocalManifest protects even damaged local markers from destructive fallback.
func HasLocalManifest(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, LocalManifestName))
	return !os.IsNotExist(err)
}

// CleanupStaleImports removes abandoned private staging directories. Symlinks
// are never followed, and directories belonging to another user are skipped.
// Call while holding the shared build lock so an active import is not removed.
func CleanupStaleImports(cacheDir string) error {
	root := filepath.Join(cacheDir, "blobs")
	cutoff := time.Now().Add(-time.Hour)
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Geteuid()) {
			return fs.SkipDir
		}
		if path != root && strings.HasPrefix(entry.Name(), ".import-") {
			if info.ModTime().Before(cutoff) {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
			}
			return fs.SkipDir
		}
		return nil
	})
}
