package imagebuild

import (
	"strings"
	"testing"
)

func TestStackPrefixReuse(t *testing.T) {
	a := StackLayer{Name: "toolchain", Digest: strings.Repeat("a", 64), ScriptDigest: strings.Repeat("b", 64)}
	b := StackLayer{Name: "package", Digest: strings.Repeat("c", 64), ScriptDigest: strings.Repeat("d", 64), Dependencies: []string{"toolchain"}}
	first, err := StackIdentities("base-manifest", []StackLayer{a, b})
	if err != nil {
		t.Fatal(err)
	}
	b.Digest = strings.Repeat("e", 64)
	changed, err := StackIdentities("base-manifest", []StackLayer{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != changed[0] || first[1] == changed[1] {
		t.Fatal("higher delta failed exact lower prefix reuse")
	}
	if _, err := StackIdentities("base", []StackLayer{b, a}); err == nil {
		t.Fatal("accepted inverted graph order")
	}
	if _, err := StackIdentities("base", []StackLayer{a, a}); err == nil {
		t.Fatal("accepted duplicate layer")
	}
	b.ScriptDigest = "version-1"
	if _, err := StackIdentities("base", []StackLayer{a, b}); err == nil {
		t.Fatal("accepted non-payload identity")
	}
}
