package microvm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/vault"
)

// Manager launches and tears down nested QEMU guests on carrier machines and
// mirrors their state into the vault.
type Manager struct {
	store        *vault.Store
	keys         *keys.Manager
	run          CarrierRunner
	runner       *exec.Runner
	ready        func(ctx context.Context, name string) error
	bootTimeout  time.Duration
	pollInterval time.Duration
}

// Option customizes a Manager.
type Option func(*Manager)

// WithCarrierRunner replaces the root-on-carrier command runner (used by tests).
func WithCarrierRunner(r CarrierRunner) Option { return func(m *Manager) { m.run = r } }

// WithBootTimeout bounds how long Launch waits for a guest to accept SSH.
func WithBootTimeout(d time.Duration) Option { return func(m *Manager) { m.bootTimeout = d } }

// WithPollInterval sets the readiness-polling interval.
func WithPollInterval(d time.Duration) Option { return func(m *Manager) { m.pollInterval = d } }

// WithReadyCheck overrides how readiness is determined (used by tests to avoid
// a real SSH round-trip).
func WithReadyCheck(f func(ctx context.Context, name string) error) Option {
	return func(m *Manager) { m.ready = f }
}

// NewManager returns a Manager backed by the vault and the real `orb` CLI.
// TCG boots are slow, so the default boot timeout is generous.
func NewManager(store *vault.Store, km *keys.Manager, opts ...Option) *Manager {
	m := &Manager{
		store:        store,
		keys:         km,
		run:          orbRoot,
		runner:       exec.NewRunnerWithResolver(NewResolver(store, km, nil)),
		bootTimeout:  10 * time.Minute,
		pollInterval: 5 * time.Second,
	}
	m.ready = m.waitSSH
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// orbRoot is the default CarrierRunner: it runs the script as root inside the
// carrier over OrbStack's multiplexer (`orb -m <carrier> -u root sh -s …`).
func orbRoot(ctx context.Context, carrier, script string, args ...string) ([]byte, []byte, error) {
	cmdArgs := append([]string{"-m", carrier, "-u", "root", "sh", "-s"}, args...)
	cmd := osexec.CommandContext(ctx, "orb", cmdArgs...)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("microvm: orb -m %s: %w: %s", carrier, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// Launch provisions a carrier (idempotently), boots a nested guest with a fresh
// vault-held SSH key, and blocks until the guest accepts SSH. On any failure it
// tears the guest down so neither QEMU processes nor vault keys leak.
func (m *Manager) Launch(ctx context.Context, spec Spec) (*vault.MicroVM, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	name := spec.Name
	if name == "" {
		name = "uvm-" + randHex(4)
	}
	img := spec.Image
	if img == "" {
		img = DefaultImage
	}
	ispec := images[img]
	mem := spec.MemoryMiB
	if mem <= 0 {
		mem = defaultMemoryMiB
	}
	cpus := spec.CPUs
	if cpus <= 0 {
		cpus = defaultCPUs
	}

	carrier, err := m.store.GetVM(spec.Carrier)
	if err != nil {
		return nil, fmt.Errorf("microvm: carrier %q: %w", spec.Carrier, err)
	}
	if carrier.State != vault.VMRunning {
		return nil, fmt.Errorf("microvm: carrier %q is not running (state %s)", spec.Carrier, carrier.State)
	}
	if !debianFamily(carrier.Distro) {
		return nil, fmt.Errorf("microvm: carrier %q distro %q unsupported; needs a debian/ubuntu carrier", spec.Carrier, carrier.Distro)
	}

	label := keyLabel(name)
	keyID, kp, err := m.keys.Generate(label, "")
	if err != nil {
		return nil, err
	}
	pub, err := kp.AuthorizedKey()
	if err != nil {
		m.keys.Revoke(label)
		return nil, err
	}

	dir := handleDir(name)
	rec := vault.MicroVM{
		ID:      name,
		Carrier: spec.Carrier,
		Image:   img,
		KeyID:   keyID,
		SSHPort: portFor(name),
		Handle:  dir,
	}
	if err := m.store.CreateMicroVM(rec); err != nil {
		m.keys.Revoke(label)
		return nil, err
	}

	script, args := ispec.bootPlan(bootParams{
		dir: dir, port: rec.SSHPort, mem: mem, cpus: cpus,
		pub: pub, url: spec.ImageURL, pkgsCSV: strings.Join(ispec.Packages, ","),
	}, true)
	if err := m.provisionAndBoot(ctx, spec.Carrier, script, args); err != nil {
		m.teardown(name, spec.Carrier, dir, label)
		return nil, err
	}
	if err := m.ready(ctx, name); err != nil {
		m.teardown(name, spec.Carrier, dir, label)
		return nil, err
	}
	if err := m.store.SetMicroVMState(name, vault.VMRunning); err != nil {
		return nil, err
	}
	return m.store.GetMicroVM(name)
}

// provisionAndBoot installs the QEMU toolchain on the carrier (idempotent) then
// runs the given boot script.
func (m *Manager) provisionAndBoot(ctx context.Context, carrier, bootScript string, args []string) error {
	if _, _, err := m.run(ctx, carrier, provisionScript); err != nil {
		return err
	}
	if _, _, err := m.run(ctx, carrier, bootScript, args...); err != nil {
		return err
	}
	return nil
}

// waitSSH polls the guest over SSH until it answers or the boot budget elapses.
func (m *Manager) waitSSH(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, m.bootTimeout)
	defer cancel()
	for {
		if _, err := m.runner.Exec(ctx, name, "true"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("microvm: %s did not become reachable over SSH: %w", name, ctx.Err())
		case <-time.After(m.pollInterval):
		}
	}
}

// Start reboots a stopped guest from its already-staged disk.
func (m *Manager) Start(ctx context.Context, name string) (*vault.MicroVM, error) {
	mv, err := m.store.GetMicroVM(name)
	if err != nil {
		return nil, err
	}
	if mv.State == vault.VMDestroyed {
		return nil, fmt.Errorf("microvm: %s is destroyed", name)
	}
	ispec, ok := images[mv.Image]
	if !ok {
		return nil, fmt.Errorf("microvm: %s has unknown image %q", name, mv.Image)
	}
	script, args := ispec.bootPlan(bootParams{
		dir: mv.Handle, port: mv.SSHPort, mem: defaultMemoryMiB, cpus: defaultCPUs,
	}, false)
	if _, _, err := m.run(ctx, mv.Carrier, script, args...); err != nil {
		return nil, err
	}
	if err := m.ready(ctx, name); err != nil {
		return nil, err
	}
	if err := m.store.SetMicroVMState(name, vault.VMRunning); err != nil {
		return nil, err
	}
	return m.store.GetMicroVM(name)
}

// Stop kills the guest's QEMU process, leaving its disk staged for a later Start.
func (m *Manager) Stop(ctx context.Context, name string) error {
	mv, err := m.store.GetMicroVM(name)
	if err != nil {
		return err
	}
	if _, _, err := m.run(ctx, mv.Carrier, stopScript, mv.Handle); err != nil {
		return err
	}
	return m.store.SetMicroVMState(name, vault.VMStopped)
}

// Destroy kills the guest and removes its working directory, optionally purging
// the guest's SSH key from the vault.
func (m *Manager) Destroy(ctx context.Context, name string, purgeKey bool) error {
	mv, err := m.store.GetMicroVM(name)
	if err != nil {
		return err
	}
	if _, _, err := m.run(ctx, mv.Carrier, destroyScript, mv.Handle); err != nil {
		return err
	}
	if err := m.store.SetMicroVMState(name, vault.VMDestroyed); err != nil {
		return err
	}
	if purgeKey && mv.KeyID != "" {
		if err := m.store.DeleteCredentialByID(mv.KeyID); err != nil && !errors.Is(err, vault.ErrNotFound) {
			return fmt.Errorf("microvm: purging key for %s: %w", name, err)
		}
	}
	return nil
}

// List returns all microVM records, newest first.
func (m *Manager) List() ([]vault.MicroVM, error) { return m.store.ListMicroVMs() }

// Get returns one microVM record.
func (m *Manager) Get(name string) (*vault.MicroVM, error) { return m.store.GetMicroVM(name) }

// teardown is best-effort cleanup on a failed Launch. It runs on a fresh
// context because the caller's may already be cancelled/timed-out.
func (m *Manager) teardown(name, carrier, dir, keyLabel string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m.run(ctx, carrier, destroyScript, dir)
	m.keys.Revoke(keyLabel)
	m.store.SetMicroVMState(name, vault.VMDestroyed)
}

// debianFamily reports whether a carrier distro uses apt (so the provision
// script applies). Distro strings may be versioned, e.g. "ubuntu:24.04".
func debianFamily(distro string) bool {
	d := strings.ToLower(distro)
	return strings.HasPrefix(d, "ubuntu") || strings.HasPrefix(d, "debian")
}

func keyLabel(name string) string { return "uvm-" + name }

// KeyLabel is the vault label of the SSH keypair for a guest name.
func KeyLabel(name string) string { return keyLabel(name) }

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
