package envcfg

import (
	"os"
	"time"
)

// defaultGuestSettle is how long a guest booted by build-image or create-base must have been up
// before the provisioning script runs. A macOS guest answers on port 22 well before its boot
// settles, and early in boot it tears down every live sshd session: on the Cirrus Sonoma base
// (2026-10-22) the script started at 18 s of guest uptime and its sshd-session was gone by 20 s,
// so build-image failed with "remote command exited without exit status or exit signal".
// 60 s is three times that measurement.
const defaultGuestSettle = 60 * time.Second

// GuestSettle returns the minimum guest uptime before provisioning. It defaults to 60s and is
// overridable via the VZ_BUILD_GUEST_SETTLE environment variable (a Go duration string); an
// empty, blank, unparseable, or non-positive value falls back to the default.
func GuestSettle() time.Duration {
	d, _ := ResolveDuration(os.Getenv("VZ_BUILD_GUEST_SETTLE"), defaultGuestSettle)
	return d
}
