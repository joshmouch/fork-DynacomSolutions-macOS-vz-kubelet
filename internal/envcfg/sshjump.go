package envcfg

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// SSHJump returns the SSH jump host that guest connections are opened through, from
// VZ_SSH_JUMP as "user@host:port" (for example josh@127.0.0.1:22). Unset or blank means
// guests are dialled directly.
//
// macOS Local Network Privacy refuses local-network connections ("no route to host") from a
// non-root launchd job that the user never approved, and a daemon cannot show the approval
// prompt. The provider runs as a user (macOS 26 guests cannot boot from root), so it reaches
// guests through the Mac's own sshd instead: loopback is not local network, and sshd (root,
// platform) opens the guest connection.
func SSHJump() (user, addr string, ok bool, err error) {
	raw := strings.TrimSpace(os.Getenv("VZ_SSH_JUMP"))
	if raw == "" {
		return "", "", false, nil
	}
	user, addr, found := strings.Cut(raw, "@")
	if !found || user == "" {
		return "", "", false, fmt.Errorf("VZ_SSH_JUMP %q is not user@host:port", raw)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", "", false, fmt.Errorf("VZ_SSH_JUMP %q is not user@host:port: %w", raw, err)
	}
	return user, addr, true, nil
}
