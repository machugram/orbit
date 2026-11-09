package vm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/vault"
)

// VMExecer runs a script as runUser inside an already-created VM, with the
// script fed on stdin and args passed as positional parameters ($1, $2, …).
// Injectable so tests run against a fake instead of the real `orb` CLI.
type VMExecer func(ctx context.Context, vmName, runUser, script string, args ...string) (stdout, stderr []byte, err error)

// orbExec is the default VMExecer: it runs `orb -m <vm> -u <user> sh -s …`
// over OrbStack's multiplexer, which grants root in every machine
// regardless of distro or isolation mode.
func orbExec(ctx context.Context, vmName, runUser, script string, args ...string) ([]byte, []byte, error) {
	cmdArgs := append([]string{"-m", vmName, "-u", runUser, "sh", "-s"}, args...)
	cmd := exec.CommandContext(ctx, "orb", cmdArgs...)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("orb -m %s -u %s: %w", vmName, runUser, err)
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// Provisioner implements the vm_create flow: ensure a warm base image for
// the distro, clone it, then inject a fresh agent keypair (spec §4 adapted).
type Provisioner struct {
	keys             *keys.Manager
	vms              *Manager
	exec             VMExecer
	user             string
	provisionTimeout time.Duration
	provisionRetries int
	provisionDelay   time.Duration
}

// ProvisionerOption customizes a Provisioner.
type ProvisionerOption func(*Provisioner)

// WithUser sets the Linux user commands run as inside VMs (default "agent").
func WithUser(u string) ProvisionerOption {
	return func(p *Provisioner) { p.user = u }
}

// WithVMExecer replaces the in-VM script runner (used by tests).
func WithVMExecer(e VMExecer) ProvisionerOption {
	return func(p *Provisioner) { p.exec = e }
}

// WithProvisionTimeout bounds total time spent provisioning sshd in a VM.
func WithProvisionTimeout(d time.Duration) ProvisionerOption {
	return func(p *Provisioner) { p.provisionTimeout = d }
}

// NewProvisioner returns a Provisioner with working defaults: user "agent",
// sshd provisioned via `orb`, and a 3-minute provisioning budget. The
// default provisioning works on every OrbStack distro family without
// configuration (see distro.go).
func NewProvisioner(km *keys.Manager, vm *Manager, opts ...ProvisionerOption) *Provisioner {
	p := &Provisioner{
		keys:             km,
		vms:              vm,
		exec:             orbExec,
		user:             "agent",
		provisionTimeout: 3 * time.Minute,
		provisionRetries: 5,
		provisionDelay:   3 * time.Second,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// User returns the Linux user the provisioner configures in VMs.
func (p *Provisioner) User() string { return p.user }

// KeyLabel returns the vault label of the keypair for a VM name.
func KeyLabel(vmName string) string { return "vm-" + vmName }

// CreateOptions customizes Provisioner.Create beyond distro/name/isolation.
type CreateOptions struct {
	// Ephemeral marks the VM for idle auto-destroy by the reaper.
	Ephemeral bool
	// TTL is the idle lifetime when Ephemeral is true. Zero means DefaultEphemeralTTL.
	TTL time.Duration
}

// DefaultEphemeralTTL is used when vm_create sets ephemeral without an explicit ttl.
const DefaultEphemeralTTL = 30 * time.Minute

// Create provisions a new machine with its own freshly generated keypair.
// Cold path: create+provision a per-distro warm base once. Warm path: clone
// the base and inject only authorized_keys for the new key.
func (p *Provisioner) Create(ctx context.Context, distro, name string, isolated bool, opts ...CreateOptions) (*vault.VM, error) {
	var opt CreateOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if name == "" {
		name = "agent-" + randHex(4)
	}
	if IsBaseName(name) {
		return nil, fmt.Errorf("vm: name %q is reserved for warm base images", name)
	}

	base := BaseName(distro)
	if err := p.ensureBase(ctx, distro, base, isolated); err != nil {
		return nil, err
	}

	label := KeyLabel(name)
	keyID, kp, err := p.keys.Generate(label, name)
	if err != nil {
		return nil, err
	}
	pub, err := kp.AuthorizedKey()
	if err != nil {
		p.keys.Revoke(label)
		return nil, err
	}

	rec, err := p.vms.Clone(ctx, base, name, distro)
	if err != nil {
		p.keys.Revoke(label)
		return nil, err
	}

	if err := p.injectKey(ctx, name, pub); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if derr := p.vms.Destroy(cleanupCtx, name, false); derr != nil {
			err = fmt.Errorf("%w (cleanup also failed: %v)", err, derr)
		}
		p.keys.Revoke(label)
		return nil, err
	}

	if err := p.vms.SetKey(name, keyID); err != nil {
		return nil, err
	}
	if opt.Ephemeral {
		ttl := opt.TTL
		if ttl <= 0 {
			ttl = DefaultEphemeralTTL
		}
		if err := p.vms.store.SetVMEphemeral(name, int64(ttl.Seconds())); err != nil {
			return nil, err
		}
		rec.Ephemeral = true
		rec.TTLSeconds = int64(ttl.Seconds())
	}
	rec.KeyID = keyID
	return rec, nil
}

// ensureBase creates and fully provisions orbit-base-<distro> once.
func (p *Provisioner) ensureBase(ctx context.Context, distro, base string, isolated bool) error {
	if rec, err := p.vms.store.GetVM(base); err == nil && rec.State != vault.VMDestroyed {
		if rec.State == vault.VMStopped {
			return p.vms.Start(ctx, base)
		}
		return nil
	}

	label := KeyLabel(base)
	keyID, kp, err := p.keys.Generate(label, base)
	if err != nil {
		return err
	}
	pub, err := kp.AuthorizedKey()
	if err != nil {
		p.keys.Revoke(label)
		return err
	}

	if _, err := p.vms.Create(ctx, CreateSpec{
		Distro:           distro,
		Name:             base,
		Username:         p.user,
		DisableIsolation: !isolated,
	}); err != nil {
		p.keys.Revoke(label)
		return fmt.Errorf("vm: ensuring base %s: %w", base, err)
	}

	if err := p.provision(ctx, base, pub); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		// Bases are protected from Destroy — delete via orbctl directly for cleanup.
		_, _, _ = p.vms.run(cleanupCtx, "delete", "-f", base)
		_ = p.vms.store.SetVMState(base, vault.VMDestroyed)
		p.keys.Revoke(label)
		return err
	}

	return p.vms.SetKey(base, keyID)
}

// injectKey installs authorized_keys on a clone (sshd already present).
func (p *Provisioner) injectKey(ctx context.Context, vmName, pubKey string) error {
	return p.runScript(ctx, vmName, keyInjectScript(), pubKey)
}

// provision runs the full sshd provisioning script as root inside the VM.
func (p *Provisioner) provision(ctx context.Context, vmName, pubKey string) error {
	return p.runScript(ctx, vmName, provisionScript(), pubKey)
}

func (p *Provisioner) runScript(ctx context.Context, vmName, script, pubKey string) error {
	ctx, cancel := context.WithTimeout(ctx, p.provisionTimeout)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt < p.provisionRetries; attempt++ {
		stdout, stderr, err := p.exec(ctx, vmName, "root", script, p.user, pubKey)
		if err == nil && bytes.Contains(stdout, []byte("ORBIT_PROVISIONED")) {
			return nil
		}
		if err != nil {
			lastErr = fmt.Errorf("provisioning %s: %w: %s", vmName, err, strings.TrimSpace(string(stderr)))
		} else {
			lastErr = fmt.Errorf("provisioning %s: script did not complete: %s", vmName, strings.TrimSpace(string(stderr)))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(p.provisionDelay):
		}
	}
	return lastErr
}
