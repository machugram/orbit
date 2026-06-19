package vm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"time"

	"orbit/internal/vault"
)

// Manager wraps the orbctl CLI and mirrors machine state into the vault.
type Manager struct {
	store         *vault.Store
	run           CmdRunner
	pollInterval  time.Duration
	createTimeout time.Duration
}

// Option customizes a Manager.
type Option func(*Manager)

// WithRunner replaces the orbctl subprocess runner (used by tests).
func WithRunner(r CmdRunner) Option {
	return func(m *Manager) { m.run = r }
}

// WithCreateTimeout bounds how long Create waits for a machine to reach
// the running state.
func WithCreateTimeout(d time.Duration) Option {
	return func(m *Manager) { m.createTimeout = d }
}

// WithPollInterval sets the state-polling interval.
func WithPollInterval(d time.Duration) Option {
	return func(m *Manager) { m.pollInterval = d }
}

// NewManager returns a Manager backed by the vault and the real orbctl CLI
// unless overridden with WithRunner.
func NewManager(store *vault.Store, opts ...Option) *Manager {
	m := &Manager{
		store:         store,
		run:           orbctl,
		pollInterval:  500 * time.Millisecond,
		createTimeout: 3 * time.Minute,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// orbctl is the default CmdRunner: a real orbctl subprocess.
func orbctl(ctx context.Context, args ...string) ([]byte, []byte, error) {
	cmd := osexec.CommandContext(ctx, "orbctl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("vm: orbctl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// Create provisions a new machine and records it in the vault. It blocks
// until the machine reports the running state or createTimeout elapses.
func (m *Manager) Create(ctx context.Context, spec CreateSpec) (*vault.VM, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	name := spec.Name
	if name == "" {
		name = "agent-" + randHex(4)
	}
	username := spec.Username
	if username == "" {
		username = "agent"
	}

	// Record first: the PK constraint rejects duplicate names before any
	// orbctl side effects.
	if err := m.store.CreateVM(name, spec.Distro); err != nil {
		return nil, err
	}

	args := []string{"create"}
	if !spec.DisableIsolation {
		args = append(args, "--isolated")
	}
	if len(spec.UserData) > 0 {
		f, err := os.CreateTemp("", "orbit-userdata-*.yaml")
		if err != nil {
			return nil, fmt.Errorf("vm: writing user data: %w", err)
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(spec.UserData); err != nil {
			f.Close()
			return nil, fmt.Errorf("vm: writing user data: %w", err)
		}
		f.Close()
		args = append(args, "--user-data", f.Name())
	}
	args = append(args, "-u", username, spec.Distro, name)

	if _, _, err := m.run(ctx, args...); err != nil {
		// Creation never happened; tombstone the record.
		m.store.SetVMState(name, vault.VMDestroyed)
		return nil, err
	}

	if err := m.waitState(ctx, name, "running"); err != nil {
		// The machine exists but never came up; keep the record in
		// "creating" so the user can inspect or destroy it.
		return nil, err
	}
	if err := m.store.SetVMState(name, vault.VMRunning); err != nil {
		return nil, err
	}
	return m.store.GetVM(name)
}

// waitState polls orbctl until the machine reports want.
func (m *Manager) waitState(ctx context.Context, name, want string) error {
	ctx, cancel := context.WithTimeout(ctx, m.createTimeout)
	defer cancel()
	for {
		state, err := m.State(ctx, name)
		if err == nil && state == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vm: %s did not reach state %q: %w (last state %q, err %v)",
				name, want, ctx.Err(), state, err)
		case <-time.After(m.pollInterval):
		}
	}
}

// State queries orbctl for the machine's current state ("running",
// "stopped", ...). It does not touch the vault.
func (m *Manager) State(ctx context.Context, id string) (string, error) {
	out, _, err := m.run(ctx, "info", id, "-f", "json")
	if err != nil {
		return "", err
	}
	return parseState(out)
}

func parseState(out []byte) (string, error) {
	var info map[string]any
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("vm: parsing orbctl info output: %w", err)
	}
	if s, ok := info["state"].(string); ok {
		return s, nil
	}
	if rec, ok := info["record"].(map[string]any); ok {
		if s, ok := rec["state"].(string); ok {
			return s, nil
		}
	}
	return "", fmt.Errorf("vm: no state in orbctl info output")
}

// Start starts a stopped machine and updates the vault record.
func (m *Manager) Start(ctx context.Context, id string) error {
	if _, err := m.store.GetVM(id); err != nil {
		return err
	}
	if _, _, err := m.run(ctx, "start", id); err != nil {
		return err
	}
	return m.store.SetVMState(id, vault.VMRunning)
}

// Stop gracefully stops a machine and updates the vault record.
func (m *Manager) Stop(ctx context.Context, id string) error {
	if _, err := m.store.GetVM(id); err != nil {
		return err
	}
	if _, _, err := m.run(ctx, "stop", id); err != nil {
		return err
	}
	return m.store.SetVMState(id, vault.VMStopped)
}

// Destroy deletes the machine and marks the record destroyed. With
// purgeKeys, the associated SSH key credential is deleted from the vault.
func (m *Manager) Destroy(ctx context.Context, id string, purgeKeys bool) error {
	rec, err := m.store.GetVM(id)
	if err != nil {
		return err
	}
	if _, _, err := m.run(ctx, "delete", "-f", id); err != nil {
		return err
	}
	if err := m.store.SetVMState(id, vault.VMDestroyed); err != nil {
		return err
	}
	if purgeKeys && rec.KeyID != "" {
		if err := m.store.DeleteCredentialByID(rec.KeyID); err != nil && err != vault.ErrNotFound {
			return fmt.Errorf("vm: purging key for %s: %w", id, err)
		}
	}
	return nil
}

// SetKey associates a vault credential (SSH keypair) with a machine record.
func (m *Manager) SetKey(id, keyID string) error {
	return m.store.SetVMKey(id, keyID)
}

// List returns all vault VM records, refreshing running/stopped states from
// orbctl best-effort (orbctl being unavailable degrades to vault state).
func (m *Manager) List(ctx context.Context) ([]vault.VM, error) {
	vms, err := m.store.ListVMs()
	if err != nil {
		return nil, err
	}

	out, _, err := m.run(ctx, "list", "-f", "json")
	if err != nil {
		return vms, nil
	}
	var machines []map[string]any
	if err := json.Unmarshal(out, &machines); err != nil {
		return vms, nil
	}
	live := make(map[string]string, len(machines))
	for _, mc := range machines {
		name, _ := mc["name"].(string)
		state, _ := mc["state"].(string)
		if name != "" {
			live[name] = state
		}
	}

	for i := range vms {
		if vms[i].State == vault.VMDestroyed {
			continue
		}
		var fresh vault.VMState
		switch live[vms[i].ID] {
		case "running":
			fresh = vault.VMRunning
		case "stopped":
			fresh = vault.VMStopped
		default:
			continue
		}
		if fresh != vms[i].State {
			if err := m.store.SetVMState(vms[i].ID, fresh); err == nil {
				vms[i].State = fresh
			}
		}
	}
	return vms, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
