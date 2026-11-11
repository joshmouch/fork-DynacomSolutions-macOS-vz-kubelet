package diskguard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/internal/buildutil"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"
)

func TestNativeGraphCacheEvictsButImportSurvives(t *testing.T) {
	root := t.TempDir()
	for _, graph := range []bool{false, true} {
		src := filepath.Join(root, "private")
		if err := os.WriteFile(src, []byte("guest"), 0600); err != nil {
			t.Fatal(err)
		}
		ref := "localhost/immutable:import"
		annotations := map[string]string{}
		if graph {
			ref = "localhost/immutable:graph"
			annotations["com.agoda.macosvz.build.input-identity"] = strings.Repeat("a", 64)
			annotations["com.agoda.macosvz.build.base-ref"] = "localhost/base:pinned"
		}
		if _, err := buildutil.CacheNativeLocal(context.Background(), buildutil.PushOptions{Ref: ref, Disk: src, Annotations: annotations}, root); err != nil {
			t.Fatal(err)
		}
		dir, err := oci.ImageDir(root, ref)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = oci.ReleaseNativeFiles(dir) })
		if oci.RebuildableLocal(dir) != graph {
			t.Fatal("import/rebuildable classification mismatch")
		}
		entries := []CacheEntry{{Dir: dir, Size: 10}}
		picked, _ := SelectEvictions(entries, map[string]bool{dir: true}, 0, 1, 0)
		if len(picked) != 0 {
			t.Fatal("live snapshot selected")
		}
		if err := EvictDir(dir); graph && err != nil {
			t.Fatal(err)
		} else if !graph && err == nil {
			t.Fatal("unrecoverable import evicted")
		}
	}
}
