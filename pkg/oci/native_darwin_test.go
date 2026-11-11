package oci

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
)

func TestNativeLocalSealAndWritableClone(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "private.img")
	if err := os.WriteFile(src, []byte("stopped guest"), 0600); err != nil {
		t.Fatal(err)
	}
	m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		Layers: []ocispec.Descriptor{{MediaType: string(MediaTypeDiskImage), Annotations: map[string]string{ocispec.AnnotationTitle: "disk.img"}}}}
	ref := "localhost/native:test"
	m, err := InstallNativeLocal(context.Background(), root, ref, m, map[string]string{"disk.img": src})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := ImageDir(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "disk.img")
	t.Cleanup(func() { _ = unix.Chflags(path, 0) })
	if !IsCached(m.Layers[0], dir) {
		t.Fatal("new snapshot not verified")
	}
	if err := os.WriteFile(path, []byte("mutation"), 0600); err == nil {
		t.Fatal("kernel allowed immutable image write")
	}
	clone := filepath.Join(root, "child.img")
	if err := unix.Clonefile(path, clone, unix.CLONE_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Chflags(clone, 0) })
	if err := unix.Chflags(clone, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clone, []byte("component delta"), 0600); err != nil {
		t.Fatal(err)
	}
	if !IsCached(m.Layers[0], dir) {
		t.Fatal("child changed parent seal")
	}
	if err := unix.Chflags(path, 0); err != nil {
		t.Fatal(err)
	}
	if IsCached(m.Layers[0], dir) {
		t.Fatal("removed immutable flag accepted")
	}
	if err := os.WriteFile(path, []byte("mutated image"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chflags(path, unix.UF_IMMUTABLE); err != nil {
		t.Fatal(err)
	}
	if IsCached(m.Layers[0], dir) {
		t.Fatal("unseal/write/reseal accepted")
	}
}
