// Note: Remove when proposal https://go-review.googlesource.com/c/crypto/+/550096 is merged
package ssh

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/trace"

	"golang.org/x/crypto/ssh"

	"github.com/agoda-com/macOS-vz-kubelet/internal/envcfg"
)

// DialContext starts a client connection to the given SSH server. It is a
// convenience function that connects to the given network address,
// initiates the SSH handshake, and then sets up a Client.
//
// The provided Context must be non-nil. If the context expires before the
// connection is complete, an error is returned. Once successfully connected,
// any expiration of the context will not affect the connection.
//
// When config.Timeout > 0 it bounds connect AND handshake as one budget,
// independent of the (longer) caller ctx (see envcfg.SSHDialTimeout).
//
// See [Dial] for additional information.
func DialContext(ctx context.Context, network, addr string, config *ssh.ClientConfig) (client *ssh.Client, err error) {
	ctx, span := trace.StartSpan(ctx, "VZSSH.Dial")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if config.Timeout > 0 {
		// Bound connect+handshake as one budget independent of the (longer) caller
		// ctx: a stalled guest login hangs NewClientConn past the TCP connect, and
		// only the caller ctx guarded the handshake before. The handshake select on
		// ctx.Done() below then observes this deadline.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, config.Timeout)
		defer cancel()
	}

	tcpCtx, tcpSpan := trace.StartSpan(ctx, "VZSSH.DialTCP")
	conn, closeJump, err := dialTransport(tcpCtx, network, addr, config)
	tcpSpan.SetStatus(err)
	tcpSpan.End()
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeJump == nil {
			return
		}
		if err != nil || client == nil {
			closeJump()
			return
		}
		// The jump connection carries this client: close it when the client ends.
		go func() { _ = client.Wait(); closeJump() }()
	}()
	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result)
	_, hsSpan := trace.StartSpan(ctx, "VZSSH.Handshake")
	go func() {
		var client *ssh.Client
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
		if err == nil {
			client = ssh.NewClient(c, chans, reqs)
		}
		select {
		case ch <- result{client, err}:
		case <-ctx.Done():
			if client != nil {
				if err := client.Close(); err != nil {
					log.G(ctx).WithError(err).Error("Failed to close client")
				}
			}
		}
	}()
	select {
	case res := <-ch:
		hsSpan.SetStatus(res.err)
		hsSpan.End()
		return res.client, res.err
	case <-ctx.Done():
		err = context.Cause(ctx)
		// Close the raw conn so a timed-out handshake can't leak it (and the
		// in-flight NewClientConn goroutine unwinds).
		_ = conn.Close()
		hsSpan.SetStatus(err)
		hsSpan.End()
		return nil, err
	}
}

// dialTransport opens the byte stream to addr: directly, or, with VZ_SSH_JUMP set, as a
// direct-tcpip channel of an SSH connection to the jump host, so the jump host's sshd opens
// the guest connection and this process only connects to the jump host (envcfg.SSHJump says
// why). The jump login reuses config's authentication under the jump user. closeJump, when
// non-nil, closes the jump connection.
func dialTransport(ctx context.Context, network, addr string, config *ssh.ClientConfig) (conn net.Conn, closeJump func(), err error) {
	d := net.Dialer{Timeout: config.Timeout}
	user, jumpAddr, ok, err := envcfg.SSHJump()
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		conn, err = d.DialContext(ctx, network, addr)
		return conn, nil, err
	}
	raw, err := d.DialContext(ctx, "tcp", jumpAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("jump host %s: %w", jumpAddr, err)
	}
	if deadline, has := ctx.Deadline(); has {
		_ = raw.SetDeadline(deadline) // bounds the jump handshake; cleared once it is up
	}
	jumpConfig := *config
	jumpConfig.User = user
	c, chans, reqs, err := ssh.NewClientConn(raw, jumpAddr, &jumpConfig)
	if err != nil {
		_ = raw.Close()
		return nil, nil, fmt.Errorf("jump host %s: %w", jumpAddr, err)
	}
	_ = raw.SetDeadline(time.Time{})
	jump := ssh.NewClient(c, chans, reqs)
	conn, err = jump.DialContext(ctx, network, addr)
	if err != nil {
		_ = jump.Close()
		return nil, nil, fmt.Errorf("via jump host %s: %w", jumpAddr, err)
	}
	return conn, func() { _ = jump.Close() }, nil
}
