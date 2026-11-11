package buildutil

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func myPolicy(max int64, forbid os.FileMode) FilePolicy {
	return FilePolicy{MaxSize: max, OwnerUID: uint32(os.Getuid()), ForbidBit: forbid}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "script.sh")
	require.NoError(t, os.WriteFile(good, []byte("echo hi"), 0o600))

	b, err := ReadFile(good, myPolicy(100, 0o077))
	require.NoError(t, err)
	assert.Equal(t, "echo hi", string(b))

	_, err = ReadFile(good, myPolicy(3, 0o077))
	assert.ErrorContains(t, err, "larger than")

	require.NoError(t, os.Chmod(good, 0o666))
	_, err = ReadFile(good, myPolicy(100, 0o022))
	assert.ErrorContains(t, err, "too permissive")

	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(good, link))
	_, err = ReadFile(link, myPolicy(100, 0))
	assert.Error(t, err, "a symlink is refused")

	for _, bad := range []string{"relative.sh", dir + "/../" + filepath.Base(dir) + "/script.sh", dir + "//script.sh", dir} {
		_, err = ReadFile(bad, myPolicy(100, 0))
		assert.Error(t, err, bad)
	}

	other := FilePolicy{MaxSize: 100, OwnerUID: uint32(os.Getuid()) + 1}
	_, err = ReadFile(good, other)
	assert.ErrorContains(t, err, "owned by")
}

func TestParseCredentials(t *testing.T) {
	c, err := ParseCredentials([]byte(`{"username":"u","password":"p"}`))
	require.NoError(t, err)
	assert.Equal(t, "u", c.Username)
	_, err = ParseCredentials([]byte(`{"identityToken":"t"}`))
	assert.NoError(t, err)
	for _, bad := range []string{`{}`, `{"username":"u"}`, `{"username":"u","password":"p","extra":1}`, `nope`} {
		_, err = ParseCredentials([]byte(bad))
		assert.Error(t, err, bad)
	}
	// a malformed file never echoes its content
	_, err = ParseCredentials([]byte(`{"password":"hunter2"`))
	assert.NotContains(t, err.Error(), "hunter2")
	assert.NotContains(t, fmt.Sprintf("%v %+v %#v", c, c, c), "p\"")
	assert.NotContains(t, fmt.Sprintf("%v", Credentials{Password: "hunter2"}), "hunter2")
}

func TestCheckRefs(t *testing.T) {
	p := []string{"registry.example.com/macos-vz/"}
	ok := func(from, to string) error { return CheckRefs(from, to, p, envcfg.ImageAllowed) }
	assert.NoError(t, ok("registry.example.com/macos-vz/a:1", "registry.example.com/macos-vz/a:2"))
	assert.Error(t, ok("registry.example.com/macos-vz/a:1", "registry.example.com/macos-vz/a:1"), "same ref")
	assert.Error(t, ok("registry.example.com/macos-vz/a:1", "registry.example.com/other/a:2"), "to outside prefix")
	assert.Error(t, ok("registry.example.com/other/a:1", "registry.example.com/macos-vz/a:2"), "from outside prefix")
	assert.Error(t, ok("registry.example.com.evil.io/macos-vz/a:1", "registry.example.com/macos-vz/a:2"), "lookalike host")
	assert.Error(t, ok("registry.example.com/macos-vz/a", "registry.example.com/macos-vz/a:2"), "untagged")
	assert.NoError(t, ok("registry.example.com/macos-vz/a@sha256:"+strings.Repeat("a", 64), "registry.example.com/macos-vz/a:2"), "immutable base digest")
	assert.Error(t, ok("registry.example.com/macos-vz/a@sha256:invalid", "registry.example.com/macos-vz/a:2"), "invalid input digest")
	assert.Error(t, ok("registry.example.com/macos-vz/a:1", "registry.example.com/macos-vz/a@sha256:"+strings.Repeat("a", 64)), "output must remain a tag")
	assert.Error(t, CheckRefs("registry.example.com/macos-vz/a:1", "registry.example.com/macos-vz/a:2", nil, envcfg.ImageAllowed), "empty allowlist is refused")
}

func TestBuildDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "build")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "stale-from-killed-run"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "stale-from-killed-run", "disk.img"), []byte("x"), 0o600))

	b, err := NewBuildDir(root)
	require.NoError(t, err)
	assert.NoDirExists(t, filepath.Join(root, "stale-from-killed-run"), "leftovers are removed")

	_, err = NewBuildDir(root)
	assert.ErrorContains(t, err, "holds the lock", "one build at a time")

	for _, name := range []string{"disk.img", "aux.img", "config.json"} {
		p, err := b.Path(name)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(b.Dir(), name), p)
	}
	for _, name := range []string{"../x", "/etc/passwd", "disk.img/../../x", "other", ""} {
		_, err := b.Path(name)
		assert.Error(t, err, name)
	}

	require.NoError(t, os.WriteFile(filepath.Join(b.Dir(), "disk.img"), []byte("x"), 0o600))
	require.NoError(t, b.Close())
	assert.NoDirExists(t, b.Dir(), "the clone dir is always removed")
	assert.NoError(t, b.Close(), "Close is idempotent")

	again, err := NewBuildDir(root)
	require.NoError(t, err, "the lock is released on Close")
	require.NoError(t, again.Close())

	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(t.TempDir(), link))
	_, err = NewBuildDir(link)
	assert.Error(t, err, "a symlinked build root is refused")
}

// fakeRegistry is a minimal in-memory OCI registry that records pushes.
type fakeRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte
	auth      string
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	switch {
	case r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/blobs/"):
		d := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if b, ok := f.blobs[d]; ok {
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			w.Header().Set("Docker-Content-Digest", d)
			return
		}
		http.NotFound(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/uploads/"):
		w.Header().Set("Location", r.URL.Path+"id")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/"):
		body, _ := io.ReadAll(r.Body)
		d := r.URL.Query().Get("digest")
		if digest.FromBytes(body).String() != d {
			http.Error(w, "digest mismatch", http.StatusBadRequest)
			return
		}
		f.blobs[d] = body
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/"):
		body, _ := io.ReadAll(r.Body)
		f.manifests[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]] = body
		w.Header().Set("Docker-Content-Digest", digest.FromBytes(body).String())
		w.WriteHeader(http.StatusCreated)
	default:
		http.NotFound(w, r)
	}
}

func TestPushStreamsWithoutTempFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	reg := &fakeRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}}
	srv := httptest.NewServer(reg)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	diskData := bytes.Repeat([]byte("macos-disk"), 300000)
	auxData := []byte("aux-data")
	cfgData, err := json.Marshal(map[string]any{
		"mediatype": "application/vnd.agoda.macosvz.config.v1+json", "os": "darwin",
		"hardwareModelData": "aGFyZHdhcmU=", "machineIdData": "bWFjaGluZQ==",
	})
	require.NoError(t, err)
	for n, b := range map[string][]byte{"disk.img": diskData, "aux.img": auxData, "config.json": cfgData} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), b, 0o600))
	}

	res, err := Push(context.Background(), PushOptions{
		Ref:         host + "/macos-vz/img:v2",
		Creds:       Credentials{Username: "u", Password: "p"},
		Disk:        filepath.Join(dir, "disk.img"),
		Aux:         filepath.Join(dir, "aux.img"),
		Config:      filepath.Join(dir, "config.json"),
		Annotations: map[string]string{"com.agoda.macosvz.build.script-sha256": "abc"},
	})
	require.NoError(t, err)

	// manifest: layout the provider pulls
	var m ocispec.Manifest
	require.NoError(t, json.Unmarshal(reg.manifests["v2"], &m))
	assert.Equal(t, res.ManifestDigest, digest.FromBytes(reg.manifests["v2"]))
	assert.Equal(t, ArtifactType, m.ArtifactType)
	require.Len(t, m.Layers, 3)
	assert.Equal(t, []string{"disk.img", "aux.img", "config.json"}, []string{
		m.Layers[0].Annotations[ocispec.AnnotationTitle], m.Layers[1].Annotations[ocispec.AnnotationTitle], m.Layers[2].Annotations[ocispec.AnnotationTitle]})
	assert.Equal(t, "abc", m.Annotations["com.agoda.macosvz.build.script-sha256"])
	assert.Equal(t, digest.FromBytes(diskData).String(), m.Layers[0].Annotations["com.agoda.macosvz.content.uncompressed-digest"])
	assert.Equal(t, fmt.Sprint(len(diskData)), m.Layers[0].Annotations["com.agoda.macosvz.content.uncompressed-size"])
	assert.Equal(t, "arm64", m.Config.Platform.Architecture)

	// every referenced blob is in the registry; the disk layer gunzips to the disk
	for _, l := range m.Layers {
		require.Contains(t, reg.blobs, l.Digest.String())
		assert.Equal(t, l.Size, int64(len(reg.blobs[l.Digest.String()])))
	}
	assert.Contains(t, reg.blobs, m.Config.Digest.String())
	zr, err := gzip.NewReader(bytes.NewReader(reg.blobs[m.Layers[0].Digest.String()]))
	require.NoError(t, err)
	got, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Equal(t, diskData, got)
	assert.Equal(t, cfgData, reg.blobs[m.Layers[2].Digest.String()], "config.json is copied byte for byte")

	// The local path must produce byte-identical descriptors and manifest, including
	// metadata, without retaining compressed blobs or copying another raw disk.
	cache := t.TempDir()
	local, err := CacheLocal(context.Background(), PushOptions{
		Ref:  host + "/macos-vz/local:v2",
		Disk: filepath.Join(dir, "disk.img"), Aux: filepath.Join(dir, "aux.img"),
		Config: filepath.Join(dir, "config.json"), Annotations: res.Manifest.Annotations,
	}, cache)
	require.NoError(t, err)
	require.Equal(t, res.ManifestDigest, local.ManifestDigest)
	require.Equal(t, res.Manifest, local.Manifest)
	cachedDir, err := oci.ImageDir(cache, host+"/macos-vz/local:v2")
	require.NoError(t, err)
	for _, layer := range local.Manifest.Layers {
		require.True(t, oci.IsCached(layer, cachedDir))
	}
	files, err := os.ReadDir(cachedDir)
	require.NoError(t, err)
	require.Len(t, files, 8, "three unpacked files, their digests, and manifest plus digest")
	require.NoFileExists(t, filepath.Join(dir, "disk.img"))

	// no temp file anywhere
	left, _ := os.ReadDir(tmp)
	assert.Empty(t, left)
}

func TestPushDataDiskHasOnlyTheDiskLayer(t *testing.T) {
	reg := &fakeRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}}
	srv := httptest.NewServer(reg)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	diskData := bytes.Repeat([]byte("data-disk"), 100000)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.img"), diskData, 0o600))

	_, err := Push(context.Background(), PushOptions{
		Ref: host + "/macos-vz/xcode:16", Creds: Credentials{Username: "u", Password: "p"}, Disk: filepath.Join(dir, "data.img"),
	})
	require.NoError(t, err)

	var m ocispec.Manifest
	require.NoError(t, json.Unmarshal(reg.manifests["16"], &m))
	assert.Equal(t, ArtifactType, m.ArtifactType)
	require.Len(t, m.Layers, 1)
	assert.Equal(t, "application/vnd.agoda.macosvz.disk.image.v1", m.Layers[0].MediaType)
	assert.Equal(t, "disk.img", m.Layers[0].Annotations[ocispec.AnnotationTitle])
	assert.Equal(t, digest.FromBytes(diskData).String(), m.Layers[0].Annotations["com.agoda.macosvz.content.uncompressed-digest"])
	assert.Equal(t, fmt.Sprint(len(diskData)), m.Layers[0].Annotations["com.agoda.macosvz.content.uncompressed-size"])
	assert.Contains(t, reg.blobs, m.Config.Digest.String())
	assert.Len(t, reg.blobs, 2, "the disk layer and the empty config only")
}

func TestPushRejectsAuxWithoutConfig(t *testing.T) {
	_, err := Push(context.Background(), PushOptions{Ref: "127.0.0.1:1/x/y:z", Disk: "d", Aux: "a"})
	assert.ErrorContains(t, err, "set together")
	_, err = Push(context.Background(), PushOptions{Ref: "127.0.0.1:1/x/y:z", Disk: "d", Config: "c"})
	assert.ErrorContains(t, err, "set together")
}

func TestPushRejectsConfigWithoutHardwareModel(t *testing.T) {
	dir := t.TempDir()
	for n, b := range map[string]string{"disk.img": "d", "aux.img": "a", "config.json": `{"os":"darwin"}`} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte(b), 0o600))
	}
	_, err := Push(context.Background(), PushOptions{Ref: "127.0.0.1:1/x/y:z", Disk: filepath.Join(dir, "disk.img"), Aux: filepath.Join(dir, "aux.img"), Config: filepath.Join(dir, "config.json")})
	assert.ErrorContains(t, err, "hardware model")
}

type failingTarget struct{}

func (failingTarget) Push(_ context.Context, _ ocispec.Descriptor, c io.Reader) error {
	_, _ = io.CopyN(io.Discard, c, 10)
	return fmt.Errorf("upload broke")
}

func TestStreamLayerStopsCompressorWhenUploadFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	require.NoError(t, os.WriteFile(p, bytes.Repeat([]byte("x"), 5<<20), 0o600))
	err := streamLayer(context.Background(), failingTarget{}, ocispec.Descriptor{Annotations: map[string]string{ocispec.AnnotationTitle: "disk.img"}}, p)
	assert.ErrorContains(t, err, "upload broke")
}
