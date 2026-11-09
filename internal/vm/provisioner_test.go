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
	for _, pm := range []string{"apt-get", "dnf", "yum", "zypper", "pacman", "apk", "xbps-install"} {
		if !strings.Contains(s, pm) {
			t.Errorf("provision script missing package manager branch for %q", pm)
		}
	}
	for _, init := range []string{"systemctl", "rc-update", "/etc/sv/sshd"} {
		if !strings.Contains(s, init) {
			t.Errorf("provision script missing init branch for %q", init)
		}
	}
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

func TestKeyInjectScriptSkipsPackages(t *testing.T) {
	s := keyInjectScript()
	if strings.Contains(s, "apt-get") || strings.Contains(s, "apk add") {
		t.Error("key inject script should not install packages")
	}
	if !strings.Contains(s, "authorized_keys") {
		t.Error("key inject script missing authorized_keys")
	}
}

func TestBaseName(t *testing.T) {
	if got := BaseName("ubuntu:24.04"); got != "orbit-base-ubuntu-24.04" {
		t.Errorf("BaseName = %q", got)
	}
	if !IsBaseName("orbit-base-alpine") {
		t.Error("expected IsBaseName")
	}
}

func TestProvisionerCreateUsesBaseThenClone(t *testing.T) {
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

	// Cold path: create base, then clone agent.
	if c := f.lastCall("create"); c == nil || c[len(c)-1] != BaseName("alpine") {
		t.Fatalf("create args = %v, want base %s", c, BaseName("alpine"))
	}
	if c := f.lastCall("clone"); c == nil || !slicesEqual(c, []string{"clone", BaseName("alpine"), "prov-1"}) {
		t.Fatalf("clone args = %v", c)
	}

	// Two scripts: full provision on base, key inject on clone.
	if fe.count() != 2 {
		t.Fatalf("scripts ran %d times, want 2", fe.count())
	}
	if fe.calls[0].vm != BaseName("alpine") {
		t.Errorf("first script on %s, want base", fe.calls[0].vm)
	}
	if !strings.Contains(fe.calls[0].script, "apk") {
		t.Error("first script should be full provision")
	}
	if fe.calls[1].vm != "prov-1" {
		t.Errorf("second script on %s, want prov-1", fe.calls[1].vm)
	}
	if strings.Contains(fe.calls[1].script, "apk add") {
		t.Error("clone path should use key-inject script, not package install")
	}

	km := keys.NewManager(s)
	kp, err := km.Load(KeyLabel("prov-1"))
	if err != nil {
		t.Fatal(err)
	}
	wantPub, _ := kp.AuthorizedKey()
	if fe.calls[1].args[1] != wantPub {
		t.Errorf("inject pubkey = %q, want %q", fe.calls[1].args[1], wantPub)
	}
}

func TestProvisionerSecondCreateReusesBase(t *testing.T) {
	f := &fakeOrb{}
	fe := &fakeExecer{}
	p, _ := testProvisioner(t, f, fe)

	if _, err := p.Create(context.Background(), "alpine", "a1", true); err != nil {
		t.Fatal(err)
	}
	createsBefore := countCalls(f, "create")
	scriptsBefore := fe.count()

	if _, err := p.Create(context.Background(), "alpine", "a2", true); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "create") != createsBefore {
		t.Error("second create should not orbctl-create another base")
	}
	if fe.count() != scriptsBefore+1 {
		t.Fatalf("second create scripts = %d, want %d (key inject only)", fe.count(), scriptsBefore+1)
	}
	if c := f.lastCall("clone"); c == nil || c[2] != "a2" {
		t.Fatalf("second clone = %v", c)
	}
}

func countCalls(f *fakeOrb, cmd string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c[0] == cmd {
			n++
		}
	}
	return n
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

	km := keys.NewManager(s)
	if _, err := km.Load(KeyLabel(BaseName("void"))); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("orphaned base key left in vault: %v", err)
	}
	vmRec, err := s.GetVM(BaseName("void"))
	if err != nil {
		t.Fatal(err)
	}
	if vmRec.State != vault.VMDestroyed {
		t.Errorf("base left in state %s after provision failure, want destroyed", vmRec.State)
	}
}

func TestProvisionerFailsWhenScriptIncomplete(t *testing.T) {
	f := &fakeOrb{}
	fe := &fakeExecer{noDone: true}
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

func TestDestroyRefusesBase(t *testing.T) {
	f := &fakeOrb{}
	m, s := testManager(t, f)
	base := BaseName("alpine")
	if err := s.CreateVM(base, "alpine"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVMState(base, vault.VMRunning); err != nil {
		t.Fatal(err)
	}
	if err := m.Destroy(context.Background(), base, true); err == nil {
		t.Fatal("expected refuse destroy of base")
	}
	if f.lastCall("delete") != nil {
		t.Error("orbctl delete should not run for base")
	}
}

func TestProvisionerRejectsBaseName(t *testing.T) {
	p, _ := testProvisioner(t, &fakeOrb{}, &fakeExecer{})
	if _, err := p.Create(context.Background(), "alpine", BaseName("alpine"), true); err == nil {
		t.Fatal("expected reject reserved base name")
	}
}
