package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agoda-com/macOS-vz-kubelet/internal/disk"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
)

// AnnotationNativeSeal describes a node-local kernel-sealed file, NOT a byte
// digest or a portable OCI blob. The descriptor digest covers this seal only.
const AnnotationNativeSeal = "com.agoda.macosvz.local-native-seal.v1"

type nativeSeal struct {
	Device     int32
	Inode      uint64
	Generation uint32
	Birth      unix.Timespec
	Change     unix.Timespec
	Size       int64
}

func readNativeSeal(path string) ([]byte, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return nil, err
	}
	if string(fs.Fstypename[:4]) != "apfs" {
		return nil, errors.New("native image seal requires APFS")
	}
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Flags&unix.UF_IMMUTABLE == 0 {
		return nil, errors.New("native image must be a regular kernel-immutable file")
	}
	return json.Marshal(nativeSeal{st.Dev, st.Ino, st.Gen, st.Btim, st.Ctim, st.Size})
}

// ReleaseNativeFiles is used only by the existing private-staging cleanup owner.
// Published snapshots retain their seals until the cache owner admits eviction.
func ReleaseNativeFiles(dir string) error {
	for _, name := range []string{"disk.img", "aux.img", "config.json"} {
		path := filepath.Join(dir, name)
		fi, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return errors.New("native staging file is not regular")
		}
		if err := unix.Chflags(path, 0); err != nil {
			return err
		}
	}
	return nil
}

func nativeCached(l ocispec.Descriptor, path string) bool {
	seal, err := readNativeSeal(path)
	return err == nil && string(seal) == l.Annotations[AnnotationNativeSeal] && digest.FromBytes(seal) == l.Digest
}

// InstallNativeLocal consumes private stopped-VM output on the cache volume.
// It does not scan disk bytes. Each layer's digest identifies its kernel seal;
// only the manifest digest is a portable cryptographic digest of actual bytes.
// No registry operation can consume these node-local descriptors.
func InstallNativeLocal(ctx context.Context, cacheDir, ref string, m ocispec.Manifest, paths map[string]string) (result ocispec.Manifest, err error) {
	dir, err := ImageDir(cacheDir, ref)
	if err != nil {
		return result, err
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		return result, fmt.Errorf("native destination exists or cannot be inspected: %s", dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return result, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dir), ".import-")
	if err != nil {
		return result, err
	}
	defer func() {
		for name := range paths {
			_ = unix.Chflags(filepath.Join(stage, name), 0)
		}
		err = errors.Join(err, os.RemoveAll(stage))
	}()
	for i := range m.Layers {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		l := &m.Layers[i]
		if err := ValidateTitle(*l); err != nil {
			return result, err
		}
		name := l.Annotations[ocispec.AnnotationTitle]
		if name == "" {
			return result, errors.New("native layer needs a fixed title")
		}
		dst := filepath.Join(stage, name)
		if err := os.Rename(paths[name], dst); err != nil {
			return result, err
		}
		fi, err := os.Lstat(dst)
		if err != nil {
			return result, err
		}
		if !fi.Mode().IsRegular() {
			return result, errors.New("native input is not a regular file")
		}
		if err := unix.Chflags(dst, unix.UF_IMMUTABLE); err != nil {
			return result, err
		}
		seal, err := readNativeSeal(dst)
		if err != nil {
			return result, err
		}
		fi, err = os.Lstat(dst)
		if err != nil {
			return result, err
		}
		l.Digest, l.Size = digest.FromBytes(seal), fi.Size()
		l.Annotations = map[string]string{ocispec.AnnotationTitle: name, AnnotationNativeSeal: string(seal)}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return result, err
	}
	meta := filepath.Join(stage, LocalManifestName)
	if err := os.WriteFile(meta, b, 0600); err != nil {
		return result, err
	}
	if err := disk.WriteDigestFile(meta, digest.FromBytes(b)); err != nil {
		return result, err
	}
	if _, _, err := ReadLocalManifest(ctx, stage); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := os.Rename(stage, dir); err != nil {
		return result, err
	}
	return m, nil
}
