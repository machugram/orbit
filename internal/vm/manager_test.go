package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/machugram/orbit/internal/vault"
)

// fakeOrb scripts orbctl behavior and records every invocation.
type fakeOrb struct {
	mu         sync.Mutex
	calls      [][]string
	infoStates []string // states returned by successive "info" calls; last repeats
	infoIdx    int
	listJSON   string
	failCreate bool
	userData   []byte // captured content of the --user-data file at create time
}

func (f *fakeOrb) run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)

	switch args[0] {
	case "create":
		for i, a := range args {
			if a == "--user-data" && i+1 < len(args) {
				f.userData, _ = os.ReadFile(args[i+1])
			}
		}
		if f.failCreate {
			return nil, []byte("boom"), errors.New("orbctl create failed")
		}
		return nil, nil, nil
	case "clone":
		if f.failCreate {
			return nil, []byte("boom"), errors.New("orbctl clone failed")
		}
		return nil, nil, nil
	case "info":
		state := "running"
		if len(f.infoStates) > 0 {
			if f.infoIdx < len(f.infoStates) {
				state = f.infoStates[f.infoIdx]
				f.infoIdx++
			} else {
				state = f.infoStates[len(f.infoStates)-1]
			}
		}
		return []byte(fmt.Sprintf(`{"name":%q,"state":%q}`, args[1], state)), nil, nil
	case "list":
		return []byte(f.listJSON), nil, nil
	case "delete", "start", "stop":
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("fake orbctl: unknown command %v", args)
}

func (f *fakeOrb) lastCall(cmd string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i][0] == cmd {
			return f.calls[i]
		}
	}
	return nil
}

func testManager(t *testing.T, f *fakeOrb) (*Manager, *vault.Store) {
	t.Helper()
	s, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	m := NewManager(s,
		WithRunner(f.run),
		WithPollInterval(time.Millisecond),
		WithCreateTimeout(time.Second),
	)
	return m, s
}

func TestCreateBuildsOrbctlArgs(t *testing.T) {
	f := &fakeOrb{}
	m, _ := testManager(t, f)

	rec, err := m.Create(context.Background(), CreateSpec{
		Distro:   "ubuntu:24.04",
		Name:     "sandbox-1",
		UserData: []byte("#cloud-config\nkey: value\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != vault.VMRunning {
		t.Errorf("state = %s, want running", rec.State)
	}

	call := f.lastCall("create")
	if call == nil {
		t.Fatal("orbctl create never invoked")
	}
	for _, want := range []string{"--isolated", "-u", "agent", "ubuntu:24.04", "sandbox-1"} {
		if !slices.Contains(call, want) {
			t.Errorf("create args missing %q: %v", want, call)
		}
	}
	if string(f.userData) != "#cloud-config\nkey: value\n" {
		t.Errorf("user-data file content = %q", f.userData)
	}

	// The temp user-data file must be gone after Create returns.
	for i, a := range call {
		if a == "--user-data" {
			if _, err := os.Stat(call[i+1]); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("user-data temp file still exists: %s", call[i+1])
			}
		}
	}
}

func TestCreateGeneratesNameAndWaitsForRunning(t *testing.T) {
	f := &fakeOrb{infoStates: []string{"creating", "creating", "running"}}
	m, _ := testManager(t, f)

	rec, err := m.Create(context.Background(), CreateSpec{Distro: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.ID, "agent-") {
		t.Errorf("generated name = %q, want agent- prefix", rec.ID)
	}
	if rec.State != vault.VMRunning {
		t.Errorf("state = %s, want running", rec.State)
	}
}

func TestCreateTimesOutIfNeverRunning(t *testing.T) {
	f := &fakeOrb{infoStates: []string{"creating"}}
	m, s := testManager(t, f)
	// Shrink the timeout for the test.
	m.createTimeout = 30 * time.Millisecond

	_, err := m.Create(context.Background(), CreateSpec{Distro: "ubuntu", Name: "stuck"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// Record stays in "creating" for inspection.
	rec, err := s.GetVM("stuck")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != vault.VMCreating {
		t.Errorf("state = %s, want creating", rec.State)
	}
}

func TestCreateFailureTombstonesRecord(t *testing.T) {
	f := &fakeOrb{failCreate: true}
	m, s := testManager(t, f)

	if _, err := m.Create(context.Background(), CreateSpec{Distro: "ubuntu", Name: "doomed"}); err == nil {
		t.Fatal("expected create error")
	}
	rec, err := s.GetVM("doomed")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != vault.VMDestroyed {
		t.Errorf("state = %s, want destroyed", rec.State)
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	m, _ := testManager(t, &fakeOrb{})
	cases := []CreateSpec{
		{Distro: "ubuntu; rm -rf /"},
		{Distro: "ubuntu", Name: "Bad Name"},
		{Distro: "ubuntu", Name: "ok", Username: "root;x"},
		{Distro: ""},
	}
	for _, spec := range cases {
		if _, err := m.Create(context.Background(), spec); err == nil {
			t.Errorf("spec %+v accepted, want validation error", spec)
		}
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	f := &fakeOrb{}
	m, _ := testManager(t, f)

	if _, err := m.Create(context.Background(), CreateSpec{Distro: "ubuntu", Name: "dup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), CreateSpec{Distro: "ubuntu", Name: "dup"}); err == nil {
		t.Error("duplicate name accepted")
	}
}

func TestStartStopDestroyLifecycle(t *testing.T) {
	f := &fakeOrb{}
	m, s := testManager(t, f)
	ctx := context.Background()

	if _, err := m.Create(ctx, CreateSpec{Distro: "ubuntu", Name: "lc"}); err != nil {
		t.Fatal(err)
	}

	if err := m.Stop(ctx, "lc"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.GetVM("lc"); rec.State != vault.VMStopped {
		t.Errorf("after stop: %s", rec.State)
	}

	if err := m.Start(ctx, "lc"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.GetVM("lc"); rec.State != vault.VMRunning {
		t.Errorf("after start: %s", rec.State)
	}

	if err := m.Destroy(ctx, "lc", false); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.GetVM("lc"); rec.State != vault.VMDestroyed {
		t.Errorf("after destroy: %s", rec.State)
	}
	if call := f.lastCall("delete"); !slices.Contains(call, "-f") {
		t.Errorf("delete not forced: %v", call)
	}

	if err := m.Start(ctx, "ghost"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("start on unknown vm: %v, want ErrNotFound", err)
	}
}

func TestDestroyPurgesKeys(t *testing.T) {
	f := &fakeOrb{}
	m, s := testManager(t, f)
	ctx := context.Background()

	if _, err := m.Create(ctx, CreateSpec{Distro: "ubuntu", Name: "purged"}); err != nil {
		t.Fatal(err)
	}
	keyID, err := s.SetCredential("ssh-keys", "vm-purged", []byte("privkey"), vault.WithVM("purged"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetKey("purged", keyID); err != nil {
		t.Fatal(err)
	}

	if err := m.Destroy(ctx, "purged", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetCredentialByID(keyID); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("key not purged: %v", err)
	}
}

func TestListRefreshesStates(t *testing.T) {
	f := &fakeOrb{listJSON: `[{"name":"drifted","state":"stopped"}]`}
	m, s := testManager(t, f)
	ctx := context.Background()

	if _, err := m.Create(ctx, CreateSpec{Distro: "ubuntu", Name: "drifted"}); err != nil {
		t.Fatal(err)
	}
	// Vault thinks it's running; orbctl reports stopped.
	vms, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].State != vault.VMStopped {
		t.Errorf("list = %+v, want drifted stopped", vms)
	}
	if rec, _ := s.GetVM("drifted"); rec.State != vault.VMStopped {
		t.Errorf("vault not updated: %s", rec.State)
	}
}
