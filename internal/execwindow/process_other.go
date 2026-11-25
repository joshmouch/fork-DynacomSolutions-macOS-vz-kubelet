//go:build !darwin

package execwindow

import "errors"

// SnapshotVMProcesses lists nothing: only macOS has VM host processes.
func SnapshotVMProcesses() map[int]bool { return map[int]bool{} }

// FindVMProcess never finds a process off macOS.
func FindVMProcess(map[int]bool) Finder {
	return func() (Meter, error) { return nil, errors.New("VM host processes exist only on macOS") }
}
