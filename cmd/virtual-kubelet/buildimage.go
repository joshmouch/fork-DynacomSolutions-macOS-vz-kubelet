package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/imagebuild"

	"github.com/sirupsen/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	logruslogger "github.com/virtual-kubelet/virtual-kubelet/log/logrus"
)

const buildImageUsage = `usage: MacOSVK build-image --from <ref> --to <ref> --script <file> (--registry-creds-file <file> | --local) [flags]

Builds a new macOS image from one already in the provider cache: boots an APFS clone of it,
runs the script in the guest as the VZ_SSH_USER user, stops the VM from the host and streams
the result to the registry. Runs as the provider's user (root, or the user the provider runs as). Needs VZ_ALLOWED_IMAGE_PREFIXES, VZ_SSH_USER and
VZ_SSH_PASSWORD or VZ_SSH_PRIVATE_KEY_PATH (or _BASE64); honours VZ_MIN_FREE_BYTES and VZ_BRIDGE_INTERFACE.
Local imports are verified from their saved manifest without contacting the base registry.
Refuses to run while a macOS virtual machine owned by root or the invoking user (the provider's) or by a --count-vm-user
user is running; Linux VMs such as Colima's do not count against Apple's 2-VM cap.
With --data-disk-gib N the result is a data disk, not a bootable image: the script finds the blank
extra disk and formats it (diskutil eraseDisk), and only that disk is pushed.
The script must leave the data disk unmountable cleanly (no open files, no processes using it): the
build unmounts it over SSH before the hard stop and fails if that does not work.
`

// buildImageMain runs the build-image subcommand and returns the process exit code.
func buildImageMain(args []string) int {
	fs := flag.NewFlagSet("build-image", flag.ContinueOnError)
	var (
		from      = fs.String("from", "", "cached base image reference (name:tag)")
		to        = fs.String("to", "", "image reference to push (name:tag)")
		script    = fs.String("script", "", "absolute path of the provisioning script (owned by the invoking user, not group/other writable)")
		creds     = fs.String("registry-creds-file", "", "absolute path of a 0600 JSON file owned by the invoking user, with username and password, or identityToken")
		local     = fs.Bool("local", false, "install the result in the cache under --to (as import-tart --local) instead of pushing it; --registry-creds-file is then optional")
		cacheDir  = fs.String("cache-dir", "", "provider cache directory (default: the provider's)")
		cpu       = fs.Uint("cpu", 4, "virtual CPUs of the build VM")
		memoryGiB = fs.Uint64("memory-gib", 8, "memory of the build VM in GiB")
		growthGiB = fs.Uint64("expected-growth-gib", 10, "disk growth of the clone expected from the script, in GiB")
		dataGiB   = fs.Uint64("data-disk-gib", 0, "build a read-only data disk instead of a bootable image: attach a blank sparse disk of this many GiB (at most 256) to the build VM (the script formats and fills it, and leaves it unmountable cleanly) and push only that disk; 0 builds a normal image")
		timeout   = fs.Duration("script-timeout", 3*time.Hour, "maximum run time of the script")
		logLevel  = fs.String("log-level", "info", "log level")
	)
	var countUsers stringList
	var attach stringList
	fs.Var(&attach, "attach-data-disk", "attach this already pulled data disk image (name:tag) read-only to the build VM (repeatable, at most 3); its size must differ from --data-disk-gib")
	fs.Var(&countUsers, "count-vm-user", "also count this user's Virtualization VMs as running macOS guests (repeatable), for example a user running tart")
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, buildImageUsage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *from == "" || *to == "" || *script == "" || (*creds == "" && !*local) {
		fs.Usage()
		return 2
	}
	lvl, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	logger := logrus.New()
	logger.SetLevel(lvl)
	log.L = logruslogger.FromLogrus(logrus.NewEntry(logger))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx = log.WithLogger(ctx, log.L)

	prefixes, err := envcfg.AllowedImagePrefixes()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dir := *cacheDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		dir = filepath.Join(base, appIdentifier)
	}
	if !filepath.IsAbs(dir) {
		fmt.Fprintln(os.Stderr, "--cache-dir must be absolute")
		return 2
	}

	err = imagebuild.Run(ctx, imagebuild.Options{
		From: *from, To: *to, ScriptPath: *script, CredsPath: *creds, CacheDir: filepath.Clean(dir), Local: *local,
		Prefixes:         prefixes,
		MinFreeBytes:     envcfg.MinFreeBytes(),
		ExpectedGrowth:   *growthGiB << 30,
		OwnerUID:         uint32(os.Geteuid()),
		CPU:              *cpu,
		MemoryBytes:      *memoryGiB << 30,
		NetworkInterface: os.Getenv("VZ_BRIDGE_INTERFACE"),
		DataDiskGiB:      *dataGiB,
		AttachDisks:      attach,
		ScriptTimeout:    *timeout,
		CountVMUsers:     countUsers,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "build-image: cancelled")
		} else {
			fmt.Fprintln(os.Stderr, "build-image failed:", err)
		}
		return 1
	}
	return 0
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return fmt.Sprint([]string(*l)) }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
