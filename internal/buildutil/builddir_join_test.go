package buildutil

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAwaitBuildDirCancellationAndJoin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "build")
	first, err := NewBuildDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AwaitBuildDir(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		next, err := AwaitBuildDir(ctx, root)
		if err == nil {
			err = next.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("joined before release: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
