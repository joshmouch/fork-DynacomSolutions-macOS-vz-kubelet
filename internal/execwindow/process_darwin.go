//go:build darwin

package execwindow

/*
#include <dlfcn.h>
#include <libproc.h>
#include <mach/mach_time.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/resource.h>
#include <unistd.h>

// vm_xpc_pids lists the processes that are Virtualization.framework's per-VM XPC service, up to max.
static int vm_xpc_pids(pid_t *out, int max) {
	int n = proc_listallpids(NULL, 0);
	if (n <= 0) return 0;
	pid_t *all = (pid_t *)calloc((size_t)n + 64, sizeof(pid_t));
	if (!all) return 0;
	n = proc_listallpids(all, (n + 64) * (int)sizeof(pid_t));
	int found = 0;
	for (int i = 0; i < n && found < max; i++) {
		char path[PROC_PIDPATHINFO_MAXSIZE];
		if (proc_pidpath(all[i], path, sizeof(path)) <= 0) continue;
		if (strstr(path, "com.apple.Virtualization.VirtualMachine") != NULL) out[found++] = all[i];
	}
	free(all);
	return found;
}

// owner_of returns the process this one runs on behalf of (the responsible process), else its parent, and says which in *how.
static pid_t owner_of(pid_t pid, int *how) {
	typedef pid_t (*resp_fn)(pid_t);
	resp_fn f = (resp_fn)dlsym(RTLD_DEFAULT, "responsibility_get_pid_responsible_for_pid");
	if (f) {
		pid_t r = f(pid);
		if (r > 0) { *how = 1; return r; }
	}
	struct proc_bsdinfo info;
	if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, sizeof(info)) == (int)sizeof(info)) { *how = 2; return (pid_t)info.pbi_ppid; }
	*how = 0;
	return 0;
}

// usage reads the process's CPU time (nanoseconds) and physical footprint (now and lifetime high-water mark, bytes).
static int usage(pid_t pid, uint64_t *cpu_ns, uint64_t *footprint, uint64_t *footprint_max) {
	struct rusage_info_v4 ru;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V4, (rusage_info_t *)&ru) != 0) return -1;
	mach_timebase_info_data_t tb;
	mach_timebase_info(&tb);
	uint64_t ticks = ru.ri_user_time + ru.ri_system_time;
	*cpu_ns = ticks / tb.denom * tb.numer + (ticks % tb.denom) * tb.numer / tb.denom;
	*footprint = ru.ri_phys_footprint;
	*footprint_max = ru.ri_lifetime_max_phys_footprint;
	return 0;
}
*/
import "C"

import (
	"fmt"
	"os"
	"time"
	"unsafe"
)

type process struct{ pid C.pid_t }

func (p process) Read() (Reading, error) {
	var cpu, footprint, footprintMax C.uint64_t
	if C.usage(p.pid, &cpu, &footprint, &footprintMax) != 0 {
		return Reading{}, fmt.Errorf("proc_pid_rusage(%d) failed: the VM host process has exited", int(p.pid))
	}
	return Reading{At: time.Now(), CPUNanos: uint64(cpu), Footprint: uint64(footprint), LifetimeMaxFootprint: uint64(footprintMax)}, nil
}

func vmProcessIDs() []int {
	buf := make([]C.pid_t, 64)
	n := int(C.vm_xpc_pids((*C.pid_t)(unsafe.Pointer(&buf[0])), C.int(len(buf))))
	out := make([]int, 0, n)
	for _, pid := range buf[:n] {
		out = append(out, int(pid))
	}
	return out
}

// SelfMeter reads this provider's own process.
func SelfMeter() (Meter, error) { return process{pid: C.pid_t(os.Getpid())}, nil }

// SnapshotVMProcesses lists the VM host processes that exist now. Take it before the VM is created.
func SnapshotVMProcesses() map[int]bool {
	seen := map[int]bool{}
	for _, pid := range vmProcessIDs() {
		seen[pid] = true
	}
	return seen
}

// FindVMProcess resolves the host process of the VM this provider created after the snapshot. Virtualization.framework runs each
// VM in its own XPC service process; this provider's is the new one it is the owner of. With two candidates and no way to tell
// them apart it reports none rather than a guess, and the window says the process was not observed.
func FindVMProcess(before map[int]bool) Finder {
	self := os.Getpid()
	var found Meter
	return func() (Meter, error) {
		if found != nil {
			return found, nil
		}
		var mine, fresh []int
		for _, pid := range vmProcessIDs() {
			if before[pid] {
				continue
			}
			fresh = append(fresh, pid)
			var how C.int
			if owner := int(C.owner_of(C.pid_t(pid), &how)); how != 0 && owner == self {
				mine = append(mine, pid)
			}
		}
		// The owner lookup is a private call that may not name this process; the only new VM host process is then the Pod's.
		if len(mine) == 0 && len(fresh) == 1 {
			mine = fresh
		}
		switch len(mine) {
		case 0:
			return nil, nil
		case 1:
			found = process{pid: C.pid_t(mine[0])}
			return found, nil
		}
		return nil, fmt.Errorf("%d new VM host processes are this provider's: cannot tell which is the Pod's", len(mine))
	}
}
