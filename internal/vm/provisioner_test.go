package vm

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/vault"
)

type execCall struct {
	vm, user, script string
	args             []string
}

// fakeExecer records provisioning invocations and scripts a fixed outcome.
type fakeExecer struct {
	mu     sync.Mutex
	calls  []execCall
	fail   bool
	noDone bool // succeed (no error) but omit the completion marker
}

func (f *fakeExecer) exec(ctx context.Context, vm, user, script string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, execCall{vm, user, script, args})
	if f.fail {
		return nil, []byte("provision boom"), errors.New("exec failed")
	}
	if f.noDone {
		return []byte("partial output"), nil, nil
	}
	return []byte("...\nORBIT_PROVISIONED\n"), nil, nil
}

func (f *fakeExecer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func testProvisioner(t *testing.T, f *fakeOrb, fe *fakeExecer) (*Provisioner, *vault.Store) {
	t.Helper()
	s, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	m := NewManager(s, WithRunner(f.run), WithPollInterval(time.Millisecond), WithCreateTimeout(time.Second))
	p := NewProvisioner(keys.NewManager(s), m, WithVMExecer(fe.exec))
	// Keep retries fast in tests.
	p.provisionRetries = 2
	p.provisionDelay = time.Millisecond
	p.provisionTimeout = 2 * time.Second
	return p, s
}

func TestProvisionScriptIsDistroAgnostic(t *testing.T) {
	s := provisionScript()
	// Package managers across the supported families.
	for _, pm := range []string{"apt-get", "dnf", "yum", "zypper", "pacman", "apk", "xbps-install"} {
		if !strings.Contains(s, pm) {
			t.Errorf("provision script missing package manager branch for %q", pm)
		}
	}
	// Init systems: systemd, OpenRC, runit.
	for _, init := range []string{"systemctl", "rc-update", "/etc/sv/sshd"} {
		if !strings.Contains(s, init) {
			t.Errorf("provision script missing init branch for %q", init)
		}
	}
	// The critical, easy-to-forget steps.
	if !strings.Contains(s, "ssh-keygen -A") {
		t.Error("provision script does not generate host keys")
	}
	if !strings.Contains(s, "/etc/shadow") {
		t.Error("provision script does not unlock the locked account")
	}
	if !strings.Contains(s, "authorized_keys") {
		t.Error("provision script does not install authorized_keys")
	}
}

func TestProvisionerCreateProvisionsAndLinksKey(t *testing.T) {
	f := &fakeOrb{}
	fe := &fakeExecer{}
	p, s := testProvisioner(t, f, fe)

	rec, err := p.Create(context.Background(), "alpine", "prov-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if rec.KeyID == "" {
		t.Fatal("VM record has no key linked")
	}

	// Provisioning must run exactly once, as root, with the agent user and
	// the generated public key as positional args.
	if fe.count() != 1 {
		t.Fatalf("provisioned %d times, want 1", fe.count())
	}
	call := fe.calls[0]
	if call.vm != "prov-1" || call.user != "root" {
		t.Errorf("provision ran as %s@%s, want root@prov-1", call.user, call.vm)
	}
	if len(call.args) != 2 || call.args[0] != "agent" {
		t.Fatalf("provision args = %v, want [agent <pubkey>]", call.args)
	}

	// The pubkey handed to the script must match the vault key for the VM.
	km := keys.NewManager(s)
	kp, err := km.Load(KeyLabel("prov-1"))
	if err != nil {
		t.Fatal(err)
	}
	wantPub, _ := kp.AuthorizedKey()
	if call.args[1] != wantPub {
		t.Errorf("provision pubkey arg = %q, want %q", call.args[1], wantPub)
	}

	byID, err := km.LoadByID(rec.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	if !kp.Private.Equal(byID.Private) {
		t.Error("vault key_id does not point at the generated private key")
	}
}

func TestProvisionerCleansUpOnCreateFailure(t *testing.T) {
	f := &fakeOrb{failCreate: true}
	fe := &fakeExecer{}
	p, s := testProvisioner(t, f, fe)

	if _, err := p.Create(context.Background(), "ubuntu", "fail-create", true); err == nil {
		t.Fatal("expected create failure")
	}
	if fe.count() != 0 {
		t.Errorf("provisioning ran despite create failure (%d calls)", fe.count())
	}
	km := keys.NewManager(s)
	if _, err := km.Load(KeyLabel("fail-create")); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("orphaned key left in vault: %v", err)
	}
}

func TestProvisionerCleansUpOnProvisionFailure(t *testing.T) {
	f := &fakeOrb{}
	fe := &fakeExecer{fail: true}
	p, s := testProvisioner(t, f, fe)

	if _, err := p.Create(context.Background(), "void", "fail-prov", true); err == nil {
		t.Fatal("expected provision failure")
	}
	if fe.count() < 2 {
		t.Errorf("expected provisioning to retry, got %d calls", fe.count())
	}

	// Key revoked and VM torn down — no orphans of either kind.
	km := keys.NewManager(s)
	if _, err := km.Load(KeyLabel("fail-prov")); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("orphaned key left in vault: %v", err)
	}
	vmRec, err := s.GetVM("fail-prov")
	if err != nil {
		t.Fatal(err)
	}
	if vmRec.State != vault.VMDestroyed {
		t.Errorf("VM left in state %s after provision failure, want destroyed", vmRec.State)
	}
}

func TestProvisionerFailsWhenScriptIncomplete(t *testing.T) {
	f := &fakeOrb{}
	fe := &fakeExecer{noDone: true} // exits 0 but never prints the marker
	p, _ := testProvisioner(t, f, fe)

	if _, err := p.Create(context.Background(), "ubuntu", "incomplete", true); err == nil {
		t.Fatal("expected failure when completion marker is absent")
	}
}

func TestProvisionerGeneratesName(t *testing.T) {
	p, _ := testProvisioner(t, &fakeOrb{}, &fakeExecer{})
	rec, err := p.Create(context.Background(), "alpine", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.ID, "agent-") {
		t.Errorf("generated name = %q", rec.ID)
	}
}
