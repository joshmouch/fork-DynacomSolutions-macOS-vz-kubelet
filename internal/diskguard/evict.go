package diskguard

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"
	"github.com/virtual-kubelet/virtual-kubelet/log"
)

// CacheEntry is one cached image directory.
type CacheEntry struct {
	Dir      string
	Size     uint64
	LastUsed time.Time
}

// AllocatedBytes sums the allocated bytes of the regular files directly in dir
// (allocated, not logical: decompressed disks are sparse).
func AllocatedBytes(dir string) (total uint64) {
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if info, err := f.Info(); err == nil && f.Type().IsRegular() {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				total += uint64(st.Blocks) * 512 // nolint: gosec
			} else {
				total += uint64(info.Size()) // nolint: gosec
			}
		}
	}
	return total
}

// SelectEvictions picks cached images to delete, least recently used first, never
// one in inUse, until needed fits above floor. ok is false if even evicting every
// candidate is not enough (the caller should then evict nothing).
func SelectEvictions(entries []CacheEntry, inUse map[string]bool, free, needed, floor uint64) (evict []CacheEntry, ok bool) {
	var cands []CacheEntry
	for _, e := range entries {
		if !inUse[e.Dir] && (!oci.HasLocalManifest(e.Dir) || oci.RebuildableLocal(e.Dir)) {
			cands = append(cands, e)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].LastUsed.Before(cands[j].LastUsed) })
	for _, e := range cands {
		if Fits(free, needed, floor) {
			break
		}
		evict = append(evict, e)
		free += e.Size
	}
	return evict, Fits(free, needed, floor)
}

// ListCache returns every image directory under root: a directory that directly
// holds files. Image directories are leaves; nothing below one is listed.
func ListCache(root string) (entries []CacheEntry) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		if oci.HasLocalManifest(p) && !oci.RebuildableLocal(p) {
			log.G(context.Background()).Infof("Protecting local-only cached image %s from eviction", p)
			return fs.SkipDir
		}
		e := CacheEntry{Dir: p, Size: AllocatedBytes(p)}
		if e.Size == 0 {
			return nil
		}
		if info, err := d.Info(); err == nil {
			e.LastUsed = info.ModTime()
		}
		entries = append(entries, e)
		return fs.SkipDir
	})
	return entries
}

// EvictDir removes the files directly inside an image directory, then the directory
// itself if it is now empty. It never recurses, so a nested image directory survives.
func EvictDir(dir string) error {
	if oci.HasLocalManifest(dir) && !oci.RebuildableLocal(dir) {
		return fmt.Errorf("refusing to evict local-only image %s", dir)
	}
	if oci.HasLocalManifest(dir) {
		if err := oci.ReleaseNativeFiles(dir); err != nil {
			return err
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, f.Name())); err != nil {
			return err
		}
	}
	_ = os.Remove(dir)
	return nil
}

// MakeRoom evicts unprotected, non-local cached images under root (LRU first) until
// needed fits above floor, then re-measures with free. It evicts nothing if even
// every candidate would not be enough. Every eviction is logged with its size.
func MakeRoom(ctx context.Context, root string, protected map[string]bool, needed, floor uint64, free func() (uint64, error)) error {
	avail, err := free()
	if err != nil {
		return err
	}
	evict, ok := SelectEvictions(ListCache(root), protected, avail, needed, floor)
	if !ok {
		return fmt.Errorf("%w: need %d bytes plus floor %d, available %d, and no unused cached image can be evicted to make room", ErrInsufficientSpace, needed, floor, avail)
	}
	// One at a time, re-measuring: clones or snapshots can keep deleted blocks
	// allocated, so stop as soon as it fits.
	for _, e := range evict {
		if Fits(avail, needed, floor) {
			return nil
		}
		log.G(ctx).Warnf("Evicting unused cached image %s (%d bytes, last used %s) to free space", e.Dir, e.Size, e.LastUsed.Format(time.RFC3339))
		if err := EvictDir(e.Dir); err != nil {
			return fmt.Errorf("failed to evict %s: %w", e.Dir, err)
		}
		before := avail
		if avail, err = free(); err != nil {
			return err
		}
		var reclaimed uint64
		if avail > before {
			reclaimed = avail - before
		}
		log.G(ctx).Infof("Cache eviction measured free-space gain: %d bytes for %s (allocated estimate %d)", reclaimed, e.Dir, e.Size)
	}
	if !Fits(avail, needed, floor) {
		return fmt.Errorf("%w: need %d bytes plus floor %d, available %d after eviction", ErrInsufficientSpace, needed, floor, avail)
	}
	return nil
}
