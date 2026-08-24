package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
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
// and key is delegated to a Resolver. Clients are pooled per target id with an
// idle TTL so bursts of exec/file ops reuse one handshake.
type Runner struct {
	resolver    Resolver
	dialTimeout time.Duration
	idleTTL     time.Duration
	dialTCP     func(ctx context.Context, network, address string) (net.Conn, error)
	onDial      func() // test hook: called once per new TCP dial

	poolMu sync.Mutex
	pool   map[string]*pooledClient
}

// RunnerOption customizes a Runner.
type RunnerOption func(*runnerConfig)

type runnerConfig struct {
	target      TargetResolver
	dialTimeout time.Duration
	idleTTL     time.Duration
	dialTCP     func(ctx context.Context, network, address string) (net.Conn, error)
	onDial      func()
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

// WithIdleTTL overrides how long an unused pooled client is retained.
func WithIdleTTL(d time.Duration) RunnerOption {
	return func(c *runnerConfig) { c.idleTTL = d }
}

// WithDialTCP replaces the TCP dialer (used by tests).
func WithDialTCP(f func(ctx context.Context, network, address string) (net.Conn, error)) RunnerOption {
	return func(c *runnerConfig) { c.dialTCP = f }
}

// WithOnDial registers a callback invoked once per new TCP dial (tests).
func WithOnDial(f func()) RunnerOption {
	return func(c *runnerConfig) { c.onDial = f }
}

// NewRunner returns a Runner that connects to each VM's own sshd at
// <vm>.orb.local:22 as the "agent" user, authenticating with the VM's
// vault-held key.
func NewRunner(store *vault.Store, km *keys.Manager, opts ...RunnerOption) *Runner {
	cfg := newRunnerConfig(opts)
	return newRunner(&vaultResolver{store: store, keys: km, target: cfg.target}, cfg)
}

// NewRunnerWithResolver returns a Runner backed by a custom Resolver. Used to
// drive targets that are not plain vault VMs (e.g. nested microVMs).
func NewRunnerWithResolver(resolver Resolver, opts ...RunnerOption) *Runner {
	return newRunner(resolver, newRunnerConfig(opts))
}

func newRunner(resolver Resolver, cfg runnerConfig) *Runner {
	return &Runner{
		resolver:    resolver,
		dialTimeout: cfg.dialTimeout,
		idleTTL:     cfg.idleTTL,
		dialTCP:     cfg.dialTCP,
		onDial:      cfg.onDial,
		pool:        make(map[string]*pooledClient),
	}
}

func newRunnerConfig(opts []RunnerOption) runnerConfig {
	cfg := runnerConfig{
		target:      defaultTarget,
		dialTimeout: DefaultDialTimeout,
		idleTTL:     DefaultIdleTTL,
	}
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

// Exec runs a shell command on the VM and returns its output and exit code.
// A non-zero exit code is reported in the Result, not as an error. Cancel
// or time out via ctx.
func (r *Runner) Exec(ctx context.Context, vmID, command string) (*Result, error) {
	return r.exec(ctx, vmID, command, nil)
}

func (r *Runner) exec(ctx context.Context, vmID, command string, stdin io.Reader) (*Result, error) {
	client, release, err := r.acquire(ctx, vmID)
	if err != nil {
		return nil, err
	}
	ok := true
	defer func() { release(ok) }()

	session, err := client.NewSession()
	if err != nil {
		ok = false
		return nil, fmt.Errorf("exec: opening session: %w", err)
	}
	defer session.Close()

	// Cancel unblocks session.Run by closing the session, not the pooled client.
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()

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
		ok = false
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
// (default "bash"), and removes it afterwards — all over one pooled SSH
// connection (the exec_script MCP tool).
func (r *Runner) ExecScript(ctx context.Context, vmID, script, interpreter string) (*Result, error) {
	if interpreter == "" {
		interpreter = "bash"
	}
	if !allowedInterpreters[interpreter] {
		return nil, fmt.Errorf("exec: unsupported interpreter %q", interpreter)
	}

	client, release, err := r.acquire(ctx, vmID)
	if err != nil {
		return nil, err
	}
	ok := true
	defer func() { release(ok) }()

	remote := fmt.Sprintf("/tmp/orbit-%s.script", randToken())
	if err := r.pushOn(client, vmID, bytes.NewReader([]byte(script)), remote, 0o700); err != nil {
		ok = false
		return nil, err
	}
	defer func() {
		// Best-effort cleanup on the same client; ignore errors.
		_, _ = r.runOn(ctx, client, vmID, "rm -f "+shellQuote(remote), nil)
	}()

	res, err := r.runOn(ctx, client, vmID, interpreter+" "+shellQuote(remote), nil)
	if err != nil {
		ok = false
		return nil, err
	}
	return res, nil
}

// runOn opens a session on an already-acquired client.
func (r *Runner) runOn(ctx context.Context, client *ssh.Client, vmID, command string, stdin io.Reader) (*Result, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("exec: opening session: %w", err)
	}
	defer session.Close()

	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()

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

// pushOn uploads via SFTP on an already-acquired client.
func (r *Runner) pushOn(client *ssh.Client, vmID string, src io.Reader, remotePath string, mode os.FileMode) error {
	sftpc, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("exec: starting sftp on %s: %w", vmID, err)
	}
	defer sftpc.Close()

	if dir := path.Dir(remotePath); dir != "." && dir != "/" {
		if err := sftpc.MkdirAll(dir); err != nil {
			return fmt.Errorf("exec: creating %s on %s: %w", dir, vmID, err)
		}
	}
	dst, err := sftpc.Create(remotePath)
	if err != nil {
		return fmt.Errorf("exec: creating %s on %s: %w", remotePath, vmID, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return fmt.Errorf("exec: uploading to %s: %w", vmID, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("exec: uploading to %s: %w", vmID, err)
	}
	if err := sftpc.Chmod(remotePath, mode); err != nil {
		return fmt.Errorf("exec: chmod %s on %s: %w", remotePath, vmID, err)
	}
	return nil
}

// shellQuote single-quotes s for safe interpolation into a shell command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
