package vm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"orbit/internal/keys"
	"orbit/internal/vault"
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

// Provisioner implements the vm_create flow: generate a keypair, create the
// machine, then install and configure sshd inside it so the agent's key
// works (spec §4, adapted — see distro.go for why cloud-init is not used).
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

// Create provisions a new machine with its own freshly generated keypair:
// orbctl creates the VM and the agent user, then a root script installs and
// enables sshd and the agent's authorized_keys. On any failure the VM and
// its key are cleaned up so neither OrbStack machines nor vault keys leak.
func (p *Provisioner) Create(ctx context.Context, distro, name string, isolated bool) (*vault.VM, error) {
	if name == "" {
		name = "agent-" + randHex(4)
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

	rec, err := p.vms.Create(ctx, CreateSpec{
		Distro:           distro,
		Name:             name,
		Username:         p.user,
		DisableIsolation: !isolated,
	})
	if err != nil {
		p.keys.Revoke(label)
		return nil, err
	}

	if err := p.provision(ctx, name, pub); err != nil {
		// The machine is up but unreachable with our key; tear it down so
		// it doesn't linger, and revoke the now-useless key.
		p.vms.Destroy(ctx, name, false)
		p.keys.Revoke(label)
		return nil, err
	}

	if err := p.vms.SetKey(name, keyID); err != nil {
		return nil, err
	}
	rec.KeyID = keyID
	return rec, nil
}

// provision runs the sshd provisioning script as root inside the VM,
// retrying to absorb the brief window where the VM is "running" but its
// network or package mirrors are not yet ready.
func (p *Provisioner) provision(ctx context.Context, vmName, pubKey string) error {
	ctx, cancel := context.WithTimeout(ctx, p.provisionTimeout)
	defer cancel()

	script := provisionScript()
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
