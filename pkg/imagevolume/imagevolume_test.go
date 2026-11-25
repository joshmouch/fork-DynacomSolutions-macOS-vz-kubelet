package imagevolume

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func layerTar(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body))}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gz(t *testing.T, b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeRepo struct {
	blobs   map[digest.Digest][]byte
	tags    map[string]ocispec.Descriptor
	fetches int
}

func (f *fakeRepo) add(mediaType string, b []byte) ocispec.Descriptor {
	d := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}
	f.blobs[d.Digest] = b
	return d
}

func (f *fakeRepo) FetchReference(_ context.Context, ref string) (ocispec.Descriptor, io.ReadCloser, error) {
	d, ok := f.tags[ref]
	if !ok {
		return d, nil, errors.New("not found")
	}
	return d, io.NopCloser(bytes.NewReader(f.blobs[d.Digest])), nil
}

func (f *fakeRepo) Fetch(_ context.Context, d ocispec.Descriptor) (io.ReadCloser, error) {
	b, ok := f.blobs[d.Digest]
	if !ok {
		return nil, errors.New("not found")
	}
	f.fetches++
	return io.NopCloser(bytes.NewReader(b)), nil
}

func image(t *testing.T, f *fakeRepo, tag string, index bool, layers ...[]byte) {
	t.Helper()
	m := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: f.add(ocispec.MediaTypeImageConfig, []byte("{}"))}
	m.SchemaVersion = 2
	for _, l := range layers {
		m.Layers = append(m.Layers, f.add(ocispec.MediaTypeImageLayerGzip, gz(t, l)))
	}
	mb, _ := json.Marshal(m)
	md := f.add(ocispec.MediaTypeImageManifest, mb)
	if !index {
		f.tags[tag] = md
		return
	}
	att := f.add(ocispec.MediaTypeImageManifest, []byte(`{"layers":[]}`))
	att.Annotations = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}
	md.Platform = &ocispec.Platform{OS: "linux", Architecture: "amd64"}
	idx := ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{att, md}}
	idx.SchemaVersion = 2
	ib, _ := json.Marshal(idx)
	f.tags[tag] = f.add(ocispec.MediaTypeImageIndex, ib)
}

func TestMaterializeSharesLowerChains(t *testing.T) {
	f := &fakeRepo{blobs: map[digest.Digest][]byte{}, tags: map[string]ocispec.Descriptor{}}
	base := layerTar(t,
		entry{name: "node_modules/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "node_modules/a/index.js", typ: tar.TypeReg, body: "a"},
		entry{name: "node_modules/.bin/a", typ: tar.TypeSymlink, link: "../a/index.js"},
		entry{name: "old.txt", typ: tar.TypeReg, body: "old"},
	)
	top1 := layerTar(t, entry{name: "src/x.ts", typ: tar.TypeReg, body: "one"}, entry{name: ".wh.old.txt", typ: tar.TypeReg})
	top2 := layerTar(t, entry{name: "src/x.ts", typ: tar.TypeReg, body: "two"}, entry{name: "src/y.ts", typ: tar.TypeLink, link: "src/x.ts"})
	image(t, f, "one", true, base, top1)
	image(t, f, "two", false, base, top2)
	s := New(t.TempDir(), 0)
	s.Fetcher = func(string, resource.RegistryCredentials) (Fetcher, error) { return f, nil }

	one, err := s.Materialize(context.Background(), "registry.example/tree:one", resource.RegistryCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(one, "src/x.ts")); string(b) != "one" {
		t.Fatalf("x.ts = %q", b)
	}
	if _, err := os.Lstat(filepath.Join(one, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("whiteout left old.txt: %v", err)
	}
	if l, _ := os.Readlink(filepath.Join(one, "node_modules/.bin/a")); l != "../a/index.js" {
		t.Fatalf("symlink = %q", l)
	}
	fetched := f.fetches
	two, err := s.Materialize(context.Background(), "registry.example/tree:two", resource.RegistryCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	if f.fetches-fetched != 1 {
		t.Fatalf("second image fetched %d layer blobs; the shared base chain should be reused", f.fetches-fetched)
	}
	if b, _ := os.ReadFile(filepath.Join(two, "src/y.ts")); string(b) != "two" {
		t.Fatalf("hard link y.ts = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(two, "old.txt")); string(b) != "old" {
		t.Fatalf("image two lost old.txt: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(one, "src/x.ts")); string(b) != "one" {
		t.Fatalf("image two changed image one's chain: %q", b)
	}
	again, err := s.Materialize(context.Background(), "registry.example/tree:one", resource.RegistryCredentials{})
	if err != nil || again != one {
		t.Fatalf("re-materialize = %q, %v", again, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Root, "tmp")); len(entries) != 0 {
		t.Fatalf("work directories left behind: %d", len(entries))
	}
}

func TestApplyRefusesEscapes(t *testing.T) {
	for name, entries := range map[string][]entry{
		"through symlink":  {{name: "out", typ: tar.TypeSymlink, link: "/tmp"}, {name: "out/evil", typ: tar.TypeReg, body: "x"}},
		"hard link escape": {{name: "l", typ: tar.TypeLink, link: "../../etc/passwd"}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			err := Apply(root, bytes.NewReader(layerTar(t, entries...)))
			if err == nil {
				t.Fatal("Apply accepted an entry that leaves the tree")
			}
		})
	}
}

func TestApplyConfinesDotDot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(root, bytes.NewReader(layerTar(t, entry{name: "../../evil", typ: tar.TypeReg, body: "x"}))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(parent, "evil")); !os.IsNotExist(err) {
		t.Fatal("an entry was written above the root")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "evil")); string(b) != "x" {
		t.Fatalf("confined entry = %q", b)
	}
}

func TestMaterializeRefusesCorruptLayer(t *testing.T) {
	f := &fakeRepo{blobs: map[digest.Digest][]byte{}, tags: map[string]ocispec.Descriptor{}}
	image(t, f, "bad", false, layerTar(t, entry{name: "a", typ: tar.TypeReg, body: "a"}))
	for d, b := range f.blobs {
		if bytes.HasPrefix(b, []byte{0x1f, 0x8b}) {
			f.blobs[d] = append(append([]byte{}, b...), 0) // trailing byte: digest mismatch
		}
	}
	s := New(t.TempDir(), 0)
	s.Fetcher = func(string, resource.RegistryCredentials) (Fetcher, error) { return f, nil }
	_, err := s.Materialize(context.Background(), "registry.example/tree:bad", resource.RegistryCredentials{})
	if err == nil || !strings.Contains(err.Error(), "layer 0") {
		t.Fatalf("corrupt layer error = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Root, "chains")); len(entries) != 0 {
		t.Fatal("a corrupt layer left a chain")
	}
}
