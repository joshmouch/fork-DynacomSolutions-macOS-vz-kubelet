package buildutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckBaseRef(t *testing.T) {
	for _, ok := range []string{
		"registry.s.dynacom.dev/macos-vz/tahoe-base:26.5",
		"registry.s.dynacom.dev/macos-vz/sub/name:tag-1",
	} {
		assert.NoError(t, CheckBaseRef(ok), ok)
	}
	for _, bad := range []string{
		"",
		"registry.s.dynacom.dev/macos-vz/",
		"registry.s.dynacom.dev/macos-vz/tahoe",                  // untagged
		"registry.s.dynacom.dev/other/tahoe:1",                   // wrong namespace
		"registry.s.dynacom.dev/macos-vzx/tahoe:1",               // prefix lookalike
		"evil.example.com/registry.s.dynacom.dev/macos-vz/a:1",   // prefix not at the start
		"registry.s.dynacom.dev/macos-vz/../other/a:1",           // traversal
		"registry.s.dynacom.dev/macos-vz/a@sha256:" + hex64("x"), // digest
		"ghcr.io/cirruslabs/macos-tahoe-vanilla:26.5",
	} {
		assert.Error(t, CheckBaseRef(bad), bad)
	}
}

func TestParseBaseCredentials(t *testing.T) {
	c, err := ParseBaseCredentials([]byte(`{"username":"u","password":"p","guest_password":"g"}`))
	require.NoError(t, err)
	assert.Equal(t, "g", c.GuestPassword)
	assert.NotContains(t, fmt.Sprintf("%v %+v %#v", c, c, c), "g\"")
	for _, bad := range []string{
		`{"username":"u","password":"p"}`, // guest_password required
		`{"username":"u","guest_password":"g"}`,
		`{"username":"u","password":"p","guest_password":"g","extra":1}`,
		`nope`,
	} {
		_, err = ParseBaseCredentials([]byte(bad))
		assert.Error(t, err, bad)
	}
	_, err = ParseBaseCredentials([]byte(`{"guest_password":"hunter2"`))
	assert.NotContains(t, err.Error(), "hunter2")
	// build-image keeps rejecting the field
	_, err = ParseCredentials([]byte(`{"username":"u","password":"p","guest_password":"g"}`))
	assert.Error(t, err)
}

func hex64(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestFetchVerified(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("ipsw bytes"))
	}))
	defer srv.Close()
	ctx := context.Background()
	dst := filepath.Join(t.TempDir(), "x.ipsw")

	// a sha mismatch fails closed and leaves nothing behind
	_, err := FetchVerified(ctx, srv.Client(), srv.URL, dst, hex64("other"))
	require.ErrorContains(t, err, "sha256 mismatch")
	for _, p := range []string{dst, dst + ".partial"} {
		_, statErr := os.Stat(p)
		assert.True(t, os.IsNotExist(statErr), p)
	}

	good := hex64("ipsw bytes")
	cached, err := FetchVerified(ctx, srv.Client(), srv.URL, dst, good)
	require.NoError(t, err)
	assert.False(t, cached)

	// a verified cached copy is not downloaded again
	before := hits.Load()
	cached, err = FetchVerified(ctx, srv.Client(), srv.URL, dst, good)
	require.NoError(t, err)
	assert.True(t, cached)
	assert.Equal(t, before, hits.Load())

	// a corrupt cached copy is replaced
	require.NoError(t, os.WriteFile(dst, []byte("corrupt"), 0o600))
	cached, err = FetchVerified(ctx, srv.Client(), srv.URL, dst, good)
	require.NoError(t, err)
	assert.False(t, cached)
	require.NoError(t, VerifySHA256(dst, good))

	// bad inputs
	_, err = FetchVerified(ctx, srv.Client(), "http://example.com/a.ipsw", dst+"2", good)
	assert.ErrorContains(t, err, "https")
	_, err = FetchVerified(ctx, srv.Client(), srv.URL, dst+"2", "ABC")
	assert.ErrorContains(t, err, "64")
}

func TestSeedDataVolume(t *testing.T) {
	root, tmpl := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpl, "users"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tmpl, "groups"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpl, "users/root.plist"), []byte("root"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tmpl, "groups/admin.plist"), []byte("group"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "private/etc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "private/etc/sudoers"), []byte("root ALL=(ALL) ALL\n#includedir /private/etc/sudoers.d\n"), 0o440))

	var chowned []string
	type app struct{ file, key, value string }
	var appended []app
	var enabled []string
	var sqlDB, sqlText []string
	h := SeedHooks{
		SQL: func(db, sql string) error {
			sqlDB, sqlText = append(sqlDB, db), append(sqlText, sql)
			return os.WriteFile(db, nil, 0o600) // sqlite3 creates the file
		},
		Chown: func(p string, uid, gid int) error {
			chowned = append(chowned, fmt.Sprintf("%s %d:%d", filepath.Base(p), uid, gid))
			return nil
		},
		AppendToArray: func(f, k, v string) error { appended = append(appended, app{filepath.Base(f), k, v}); return nil },
		EnableService: func(f, l string) error { enabled = append(enabled, l); return nil },
	}
	s := SeedSpec{User: "admin", Password: "s3cret&<pw>", UID: 501, GID: 20, AuthorizedKey: "ssh-ed25519 AAAA key", TemplateDir: tmpl}
	require.NoError(t, SeedDataVolume(root, s, h))

	db := filepath.Join(root, "private/var/db")
	user, err := os.ReadFile(filepath.Join(db, "dslocal/nodes/Default/users/admin.plist"))
	require.NoError(t, err)
	assert.Contains(t, string(user), "<key>ShadowHashData</key>")
	assert.Contains(t, string(user), "<string>501</string>")
	assert.NotContains(t, string(user), "s3cret")
	assert.FileExists(t, filepath.Join(db, "dslocal/nodes/Default/users/root.plist"), "template copied")
	assert.FileExists(t, filepath.Join(db, ".AppleSetupDone"))
	launchd, _ := os.ReadFile(filepath.Join(db, "com.apple.xpc.launchd/disabled.plist"))
	assert.Contains(t, string(launchd), "com.openssh.sshd")
	key, _ := os.ReadFile(filepath.Join(root, "Users/admin/.ssh/authorized_keys"))
	assert.Equal(t, "ssh-ed25519 AAAA key\n", string(key))
	assert.Contains(t, chowned, "authorized_keys 501:20")
	assert.Contains(t, chowned, "admin.plist 0:0")

	// admin group: name in users, the user's GeneratedUID in groupmembers
	guid := regexp.MustCompile(`<key>generateduid</key><array><string>([0-9A-F-]{36})</string>`).FindStringSubmatch(string(user))
	require.Len(t, guid, 2)
	assert.Equal(t, []app{{"admin.plist", "users", "admin"}, {"admin.plist", "groupmembers", guid[1]}}, appended)
	assert.Empty(t, enabled, "no disabled.plist existed: written directly")

	// sudoers.d and the sshd drop-in: contents and modes
	for path, want := range map[string]struct {
		body string
		mode os.FileMode
	}{
		"private/etc/sudoers.d/dynacom-admin":            {"admin ALL=(ALL) NOPASSWD: ALL\n", 0o440},
		"private/etc/ssh/sshd_config.d/000-dynacom.conf": {"PasswordAuthentication no\nKbdInteractiveAuthentication no\n", 0o644},
	} {
		b, err := os.ReadFile(filepath.Join(root, path))
		require.NoError(t, err, path)
		assert.Equal(t, want.body, string(b), path)
		fi, _ := os.Stat(filepath.Join(root, path))
		assert.Equal(t, want.mode, fi.Mode().Perm(), path)
	}
	assert.Contains(t, chowned, "dynacom-admin 0:0")
	assert.Contains(t, chowned, "000-dynacom.conf 0:0")

	// TCC.db: seeded once, with the schema when absent
	tcc := filepath.Join(root, "Library/Application Support/com.apple.TCC/TCC.db")
	assert.Equal(t, []string{tcc}, sqlDB)
	require.Len(t, sqlText, 1)
	for _, want := range []string{"CREATE TABLE access", "insert into admin values('version',30)", "kTCCServiceSystemPolicyAllFiles", "kTCCServicePostEvent", "/usr/libexec/sshd-keygen-wrapper"} {
		assert.Contains(t, sqlText[0], want)
	}
	assert.Contains(t, chowned, "TCC.db 0:0")

	// never overwrite an existing user
	assert.ErrorContains(t, SeedDataVolume(root, s, h), "already exists")
	// no local node and no template: refuse
	s.TemplateDir = ""
	assert.Error(t, SeedDataVolume(t.TempDir(), s, h))
}

func TestSeedDataVolumeKeepsExistingTCCDB(t *testing.T) {
	root := t.TempDir()
	node := filepath.Join(root, "private/var/db/dslocal/nodes/Default")
	require.NoError(t, os.MkdirAll(filepath.Join(node, "users"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(node, "groups"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(node, "users/root.plist"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(node, "groups/admin.plist"), nil, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "private/etc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "private/etc/sudoers"), []byte("@includedir /etc/sudoers.d\n"), 0o440))
	tcc := filepath.Join(root, "Library/Application Support/com.apple.TCC/TCC.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(tcc), 0o755))
	require.NoError(t, os.WriteFile(tcc, []byte("x"), 0o600))
	var sqlText string
	h := SeedHooks{
		Chown:         func(string, int, int) error { return nil },
		AppendToArray: func(string, string, string) error { return nil },
		EnableService: func(string, string) error { return nil },
		SQL:           func(db, sql string) error { sqlText = sql; return nil },
	}
	require.NoError(t, SeedDataVolume(root, SeedSpec{User: "admin", Password: "pw", UID: 501, GID: 20, AuthorizedKey: "k"}, h))
	assert.Contains(t, sqlText, "kTCCServicePostEvent")
	assert.NotContains(t, sqlText, "CREATE TABLE")
	fi, _ := os.Stat(tcc)
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())
}

func TestSeedDataVolumeEditsExistingLaunchdPlist(t *testing.T) {
	root := t.TempDir()
	node := filepath.Join(root, "private/var/db/dslocal/nodes/Default")
	require.NoError(t, os.MkdirAll(filepath.Join(node, "users"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(node, "groups"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(node, "users/root.plist"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(node, "groups/admin.plist"), nil, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "private/var/db/com.apple.xpc.launchd"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "private/var/db/com.apple.xpc.launchd/disabled.plist"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "private/etc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "private/etc/sudoers"), []byte("@includedir /etc/sudoers.d\n"), 0o440))
	var enabled []string
	h := SeedHooks{
		Chown:         func(string, int, int) error { return nil },
		AppendToArray: func(string, string, string) error { return nil },
		EnableService: func(f, l string) error { enabled = append(enabled, filepath.Base(f)+" "+l); return nil },
		SQL:           func(db, _ string) error { return os.WriteFile(db, nil, 0o600) },
	}
	s := SeedSpec{User: "admin", Password: "pw", UID: 501, GID: 20, AuthorizedKey: "k"}
	require.NoError(t, SeedDataVolume(root, s, h))
	assert.Equal(t, []string{"disabled.plist com.openssh.sshd"}, enabled)
}

func TestSeedRefusesSudoersWithoutInclude(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "private/etc"), 0o755))
	h := SeedHooks{Chown: func(string, int, int) error { return nil }}
	s := SeedSpec{User: "admin", Password: "pw", UID: 501, GID: 20, AuthorizedKey: "k", TemplateDir: t.TempDir()}
	// no sudoers at all
	assert.ErrorContains(t, SeedDataVolume(root, s, h), "sudoers")
	// sudoers without the include (a commented line is not an include)
	require.NoError(t, os.WriteFile(filepath.Join(root, "private/etc/sudoers"), []byte("# includedir /private/etc/sudoers.d\nroot ALL=(ALL) ALL\n"), 0o440))
	assert.ErrorContains(t, SeedDataVolume(root, s, h), "does not include")
}

func TestCheckBaseArgs(t *testing.T) {
	to := "registry.s.dynacom.dev/macos-vz/tahoe-base:26.5"
	sum := hex64("x")
	url := "https://updates.cdn-apple.com/a.ipsw"
	assert.NoError(t, CheckBaseArgs(url, sum, to, 80<<30))
	assert.Error(t, CheckBaseArgs("http://example.com/a.ipsw", sum, to, 80<<30), "plain http")
	assert.Error(t, CheckBaseArgs("", sum, to, 80<<30))
	assert.Error(t, CheckBaseArgs(url, "", to, 80<<30))
	assert.Error(t, CheckBaseArgs(url, sum, "ghcr.io/x/y:1", 80<<30))
	assert.Error(t, CheckBaseArgs(url, sum, to, 8<<30), "disk too small")
}

func TestHdiutilParsing(t *testing.T) {
	d, err := ParseAttach([]byte(`{"system-entities":[{"dev-entry":"/dev/disk7","content-hint":"GUID_partition_scheme"},{"dev-entry":"/dev/disk7s1"}]}`))
	require.NoError(t, err)
	assert.Equal(t, "/dev/disk7", d)
	_, err = ParseAttach([]byte(`{"system-entities":[{"dev-entry":"/dev/disk7s1"}]}`))
	assert.Error(t, err)
	_, err = ParseAttach([]byte(`garbage`))
	assert.Error(t, err)

	info := []byte(`{"images":[{"image-path":"/a/disk.img","system-entities":[{"dev-entry":"/dev/disk9"},{"dev-entry":"/dev/disk9s1"}]},
	  {"image-path":"/b/disk.img","system-entities":[{"dev-entry":"/dev/disk3"}]}]}`)
	d, err = FindImageDevice(info, "/b/disk.img")
	require.NoError(t, err)
	assert.Equal(t, "/dev/disk3", d)
	d, _ = FindImageDevice(info, "/none")
	assert.Empty(t, d)
}
