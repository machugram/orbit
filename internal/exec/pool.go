package exec

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// DefaultIdleTTL is how long an unused pooled SSH client is kept before
// the next acquire dials a fresh connection.
const DefaultIdleTTL = 60 * time.Second

type pooledClient struct {
	client   *ssh.Client
	refs     int
	lastUsed time.Time
}

// acquire returns a live SSH client for vmID, creating one if needed.
// The caller must invoke the returned release function exactly once.
// Pass ok=false to drop the client from the pool (e.g. after a transport error).
func (r *Runner) acquire(ctx context.Context, vmID string) (*ssh.Client, func(ok bool), error) {
	r.poolMu.Lock()
	if e, ok := r.pool[vmID]; ok {
		expired := e.refs == 0 && r.idleTTL > 0 && time.Since(e.lastUsed) > r.idleTTL
		if !expired {
			e.refs++
			client := e.client
			r.poolMu.Unlock()
			return client, func(ok bool) { r.release(vmID, client, ok) }, nil
		}
		delete(r.pool, vmID)
		_ = e.client.Close()
	}
	r.poolMu.Unlock()

	client, err := r.dial(ctx, vmID)
	if err != nil {
		return nil, nil, err
	}

	r.poolMu.Lock()
	if e, ok := r.pool[vmID]; ok {
		// Another goroutine won the race; use theirs and close ours.
		e.refs++
		shared := e.client
		r.poolMu.Unlock()
		_ = client.Close()
		return shared, func(ok bool) { r.release(vmID, shared, ok) }, nil
	}
	if r.pool == nil {
		r.pool = make(map[string]*pooledClient)
	}
	r.pool[vmID] = &pooledClient{client: client, refs: 1, lastUsed: time.Now()}
	r.poolMu.Unlock()
	return client, func(ok bool) { r.release(vmID, client, ok) }, nil
}

func (r *Runner) release(vmID string, client *ssh.Client, ok bool) {
	r.poolMu.Lock()
	defer r.poolMu.Unlock()
	e, exists := r.pool[vmID]
	if !exists || e.client != client {
		_ = client.Close()
		return
	}
	e.refs--
	if e.refs < 0 {
		e.refs = 0
	}
	e.lastUsed = time.Now()
	if !ok {
		delete(r.pool, vmID)
		_ = e.client.Close()
	}
}

// Invalidate drops any pooled SSH client for vmID. Safe to call when the
// machine is stopped or destroyed.
func (r *Runner) Invalidate(vmID string) {
	if r == nil {
		return
	}
	r.poolMu.Lock()
	defer r.poolMu.Unlock()
	if e, ok := r.pool[vmID]; ok {
		delete(r.pool, vmID)
		_ = e.client.Close()
	}
}

func (r *Runner) dial(ctx context.Context, vmID string) (*ssh.Client, error) {
	info, err := r.resolver.Resolve(ctx, vmID)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User: info.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(info.Signer)},
		// The host key is generated at first boot and not pre-distributed; all
		// traffic stays on the local machine (loopback via .orb.local, or a
		// carrier-local port for microVMs), so trust-on-connect is acceptable.
		// Bounded, documented decision — see
		// docs/adr/ADR-011-ssh-host-key-trust-model.md. TOFU pinning is
		// ROADMAP §3, required before any target is reachable off-loopback.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         r.dialTimeout,
	}

	dial := r.dialTCP
	if dial == nil {
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: r.dialTimeout}
			return d.DialContext(ctx, network, address)
		}
	}
	if r.onDial != nil {
		r.onDial()
	}
	conn, err := dial(ctx, "tcp", info.Addr)
	if err != nil {
		return nil, fmt.Errorf("exec: dialing %s: %w", info.Addr, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, info.Addr, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("exec: ssh handshake with %s as %s: %w", info.Addr, info.User, err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// poolSize is for tests.
func (r *Runner) poolSize() int {
	r.poolMu.Lock()
	defer r.poolMu.Unlock()
	return len(r.pool)
}
