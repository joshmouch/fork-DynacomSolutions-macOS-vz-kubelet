//go:build darwin

package main

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
*/
import "C"

import (
	"os"
	"runtime"
)

// The main goroutine starts on the process's main thread; keep it there.
func init() { runtime.LockOSThread() }

// main serves the main thread's run loop, and with it the main dispatch queue, while command does
// the work. Virtualization.framework posts work there: with no main run loop, a macOS 14 guest
// on a macOS 26.6 host ("Limiting display port count for old guest") restarted itself every
// ~13 s, a few seconds after its WindowServer set the display mode, while the same disk stayed
// up under Tart, which always runs one (2026-10-22).
func main() {
	go func() {
		command()
		os.Exit(0)
	}()
	C.CFRunLoopRun()
}
