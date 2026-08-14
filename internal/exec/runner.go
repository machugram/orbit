package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/vault"
)

// TargetResolver maps a VM id to the SSH endpoint and user to connect as.
type TargetResolver func(vmID string) (addr, user string)

// defaultTarget reaches the in-VM sshd via OrbStack's per-machine DNS.
func defaultTarget(vmID string) (addr, user string) {
	return vmID + ".orb.local:22", "agent"
}

// ConnInfo is everything needed to open one SSH session to a target. Signer
// holds a private key decrypted into memory only for the connection's lifetime.
type ConnInfo struct {
	Addr   string
	User   string
	Signer ssh.Signer
}

// Resolver yields connection info for a target id. The default implementation
// resolves agent-managed VMs from the vault; other implementations (e.g. the
// microvm package) can map an id to a nested guest reachable through a carrier,
// so the same Runner drives both without duplicating the SSH plumbing.
type Resolver interface {
	Resolve(ctx context.Context, id string) (ConnInfo, error)
}

// Runner executes commands on a target over SSH. How an id maps to an endpoint
// and key is delegated to a Resolver.
type Runner struct {
	resolver    Resolver
	dialTimeout time.Duration
}

// RunnerOption customizes a Runner.
type RunnerOption func(*runnerConfig)

type runnerConfig struct {
	target      TargetResolver
	dialTimeout time.Duration
}

// WithTarget overrides how VM ids resolve to SSH endpoints (used by tests
// and non-default network setups). Only affects the default vault resolver.
func WithTarget(f TargetResolver) RunnerOption {
	return func(c *runnerConfig) { c.target = f }
}

// WithDialTimeout overrides the TCP+handshake timeout.
func WithDialTimeout(d time.Duration) RunnerOption {
	return func(c *runnerConfig) { c.dialTimeout = d }
}

// NewRunner returns a Runner that connects to each VM's own sshd at
// <vm>.orb.local:22 as the "agent" user, authenticating with the VM's
// vault-held key.
func NewRunner(store *vault.Store, km *keys.Manager, opts ...RunnerOption) *Runner {
	cfg := newRunnerConfig(opts)
	return &Runner{
		resolver:    &vaultResolver{store: store, keys: km, target: cfg.target},
		dialTimeout: cfg.dialTimeout,
	}
}

// NewRunnerWithResolver returns a Runner backed by a custom Resolver. Used to
// drive targets that are not plain vault VMs (e.g. nested microVMs).
func NewRunnerWithResolver(resolver Resolver, opts ...RunnerOption) *Runner {
	cfg := newRunnerConfig(opts)
	return &Runner{resolver: resolver, dialTimeout: cfg.dialTimeout}
}

func newRunnerConfig(opts []RunnerOption) runnerConfig {
	cfg := runnerConfig{target: defaultTarget, dialTimeout: DefaultDialTimeout}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// VaultResolver returns the default Resolver: it looks up an agent-managed VM
// in the vault and reaches it at <vm>.orb.local:22. Exposed so other resolvers
// can chain to it as a fallback.
func VaultResolver(store *vault.Store, km *keys.Manager, opts ...RunnerOption) Resolver {
	cfg := newRunnerConfig(opts)
	return &vaultResolver{store: store, keys: km, target: cfg.target}
}

// vaultResolver maps a VM id to its vault-held key and OrbStack endpoint.
type vaultResolver struct {
	store  *vault.Store
	keys   *keys.Manager
	target TargetResolver
}

func (vr *vaultResolver) Resolve(ctx context.Context, vmID string) (ConnInfo, error) {
	rec, err := vr.store.GetVM(vmID)
	if err != nil {
		return ConnInfo{}, err
	}
	if rec.State == vault.VMDestroyed {
		return ConnInfo{}, fmt.Errorf("exec: vm %s is destroyed", vmID)
	}
	if rec.KeyID == "" {
		return ConnInfo{}, fmt.Errorf("exec: vm %s has no associated key", vmID)
	}
	signer, err := vr.keys.SignerByID(rec.KeyID)
	if err != nil {
		return ConnInfo{}, err
	}
	addr, user := vr.target(vmID)
	return ConnInfo{Addr: addr, User: user, Signer: signer}, nil
}

// connect resolves the target and dials its sshd. The decrypted private key
// exists only in this process's memory for the lifetime of the connection.
func (r *Runner) connect(ctx context.Context, vmID string) (*ssh.Client, error) {
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

	d := net.Dialer{Timeout: r.dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", info.Addr)
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

// Exec runs a shell command on the VM and returns its output and exit code.
// A non-zero exit code is reported in the Result, not as an error. Cancel
// or time out via ctx.
func (r *Runner) Exec(ctx context.Context, vmID, command string) (*Result, error) {
	return r.exec(ctx, vmID, command, nil)
}

func (r *Runner) exec(ctx context.Context, vmID, command string, stdin io.Reader) (*Result, error) {
	client, err := r.connect(ctx, vmID)
	if err != nil {
		return nil, err
	}
	// closeClient is idempotent: both the ctx-cancellation hook and the
	// deferred teardown call it, but the connection is closed exactly once.
	var closeOnce sync.Once
	closeClient := func() { closeOnce.Do(func() { client.Close() }) }
	// Tear the connection down if ctx ends so session.Run unblocks. stop()
	// cancels the hook on the normal path so it never runs after we return.
	stop := context.AfterFunc(ctx, closeClient)
	defer stop()
	defer closeClient()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("exec: opening session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	if stdin != nil {
		session.Stdin = stdin
	}

	res := &Result{VMID: vmID}
	err = session.Run(command)
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()

	var exitErr *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitStatus()
	default:
		if ctx.Err() != nil {
			return nil, fmt.Errorf("exec: command on %s: %w", vmID, ctx.Err())
		}
		return nil, fmt.Errorf("exec: command on %s: %w", vmID, err)
	}
	return res, nil
}

// allowedInterpreters bounds the exec_script interpreter to a fixed set, so a
// caller-supplied value cannot smuggle extra arguments or commands into the
// shell line that runs the uploaded script.
var allowedInterpreters = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "dash": true, "ash": true,
	"python3": true, "python": true, "perl": true, "ruby": true, "node": true,
}

// ExecScript uploads a script to the VM, runs it with the interpreter
// (default "bash"), and removes it afterwards (the exec_script MCP tool).
func (r *Runner) ExecScript(ctx context.Context, vmID, script, interpreter string) (*Result, error) {
	if interpreter == "" {
		interpreter = "bash"
	}
	if !allowedInterpreters[interpreter] {
		return nil, fmt.Errorf("exec: unsupported interpreter %q", interpreter)
	}
	remote := fmt.Sprintf("/tmp/orbit-%s.script", randToken())
	if err := r.PushBytes(ctx, vmID, []byte(script), remote, 0o700); err != nil {
		return nil, err
	}
	// Clean up with a context detached from ctx: if the script run was
	// cancelled or timed out, the cleanup still needs a live context to dial,
	// otherwise the temp file would linger on the VM.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.dialTimeout)
		defer cancel()
		r.Exec(cleanupCtx, vmID, "rm -f "+shellQuote(remote)) // best-effort cleanup
	}()

	return r.Exec(ctx, vmID, interpreter+" "+shellQuote(remote))
}

// shellQuote single-quotes s for safe interpolation into a shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
