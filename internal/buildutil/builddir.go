package buildutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"
)

// fixedNames are the only files a build directory holds; callers never supply a file name.
var fixedNames = map[string]bool{"disk.img": true, "aux.img": true, "config.json": true, "data.img": true}

// BuildDir is a private working directory under <root>, on the same volume as the image cache
// so APFS clonefile works. Only one build runs at a time (flock on <root>/.lock); leftovers of
// a build killed hard are removed when the next one starts.
type BuildDir struct {
	dir  string
	lock *os.File
}

// NewBuildDir creates the build directory under root (created 0700 if missing; a symlink at
// root is refused).
func NewBuildDir(root string) (*BuildDir, error) {
	return newBuildDir(context.Background(), root, false)
}

// AwaitBuildDir joins the existing build lock with cancellation. The caller
// rechecks its immutable identity after admission, so followers reuse completion.
func AwaitBuildDir(ctx context.Context, root string) (*BuildDir, error) {
	return newBuildDir(ctx, root, true)
}

func newBuildDir(ctx context.Context, root string, wait bool) (*BuildDir, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("build root %q must be a real directory", root)
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !wait || (!errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN)) {
			lock.Close()
			return nil, fmt.Errorf("another build-image run holds the lock in %q: %w", root, err)
		}
		select {
		case <-ctx.Done():
			lock.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := oci.CleanupStaleImports(filepath.Dir(root)); err != nil {
		lock.Close()
		return nil, err
	}
	// Holding the lock, anything else in root is a leftover of a killed run.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.Name() != ".lock" {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
	dir, err := os.MkdirTemp(root, "build-")
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &BuildDir{dir: dir, lock: lock}, nil
}

// Path returns the path of one of the fixed build files.
func (b *BuildDir) Path(name string) (string, error) {
	if !fixedNames[name] {
		return "", fmt.Errorf("%q is not a build file", name)
	}
	return filepath.Join(b.dir, name), nil
}

// Dir is the build directory.
func (b *BuildDir) Dir() string { return b.dir }

// Close deletes the build directory and releases the lock. It is safe to call twice.
func (b *BuildDir) Close() error {
	if b.lock == nil {
		return nil
	}
	err := os.RemoveAll(b.dir)
	b.lock.Close()
	b.lock = nil
	return err
}
