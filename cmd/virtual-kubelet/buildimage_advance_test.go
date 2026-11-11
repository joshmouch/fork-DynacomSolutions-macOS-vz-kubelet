package main

import "testing"

func TestBuildImageAdvanceArguments(t *testing.T) {
	for _, extra := range [][]string{
		{"--layers-file", "/unread/stack.json", "--max-new-layers", "-1"},
		{"--script", "/unread/component.sh", "--max-new-layers", "1"},
	} {
		args := append([]string{"--from", "registry/base:1", "--to", "registry/runner:1", "--local"}, extra...)
		if code := buildImageMain(args); code != 2 {
			t.Fatalf("invalid bounded-stack arguments returned %d instead of usage refusal", code)
		}
	}
}
