package microvm

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/vault"
)

// fakeCarrier records every root-on-carrier invocation and can be told to fail
// the call whose script contains a marker substring.
type fakeCarrier struct {
	mu      sync.Mutex
	calls   []carrierCall
	failOn  string
	failErr error
}

type carrierCall struct {
	carrier string
	script  string
	args    []string
}

func (f *fakeCarrier) run(ctx context.Context, carrier, script string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, carrierCall{carrier, script, append([]string(nil), args...)})
	if f.failOn != "" && strings.Contains(script, f.failOn) {
		return nil, nil, f.failErr
	}
	return nil, nil, nil
}

func (f *fakeCarrier) find(marker string) (carrierCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c.script, marker) {
			return c, true
		}
	}
	return carrierCall{}, false
}

// script markers unique to each carrier-side script.
const (
	markProvision   = "apt-get install"
	markLaunchCloud = "cloud-localds"    // cloud-image launch only
	markLaunchDK    = "mkfs.ext4"        // direct-kernel launch only
	markStartDK     = "no staged rootfs" // direct-kernel restart only
	markDestroy     = "rm -rf"
	markStop        = `rm -f "$DIR/qemu.pid"`
)

func testManager(t *testing.T, fc *fakeCarrier, ready func(context.Context, string) error) (*Manager, *vault.Store, *keys.Manager) {
	t.Helper()
	s, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	km := keys.NewManager(s)
	m := NewManager(s, km, WithCarrierRunner(fc.run), WithReadyCheck(ready))
	return m, s, km
}

func runningCarrier(t *testing.T, s *vault.Store, name, distro string) {
	t.Helper()
	if err := s.CreateVM(name, distro); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVMState(name, vault.VMRunning); err != nil {
		t.Fatal(err)
	}
}

func okReady(context.Context, string) error { return nil }

func TestLaunchDirectKernelDefault(t *testing.T) {
	fc := &fakeCarrier{}
	m, s, km := testManager(t, fc, okReady)
	runningCarrier(t, s, "carrier", "ubuntu")

	// No image => default "alpine" => light direct-kernel boot.
	rec, err := m.Launch(context.Background(), Spec{Name: "uvm-dk", Carrier: "carrier"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != vault.VMRunning || rec.Image != "alpine" {
		t.Errorf("rec = %+v, want running alpine", rec)
	}
	if rec.SSHPort != portFor("uvm-dk") {
		t.Errorf("port = %d, want %d", rec.SSHPort, portFor("uvm-dk"))
	}

	if _, ok := fc.find(markProvision); !ok {
		t.Fatal("provision script never ran")
	}
	if _, ok := fc.find(markLaunchCloud); ok {
		t.Error("default launch used the heavy cloud-image path")
	}
	launch, ok := fc.find(markLaunchDK)
	if !ok {
		t.Fatal("direct-kernel launch script never ran")
	}
	// Direct-kernel launch args: dir, port, mem, cpus, arch, ver, mirror, pub, pkgs.
	if launch.args[0] != handleDir("uvm-dk") {
		t.Errorf("launch dir arg = %q", launch.args[0])
	}
	if launch.args[1] != strconv.Itoa(portFor("uvm-dk")) {
		t.Errorf("launch port arg = %q", launch.args[1])
	}
	if !strings.HasPrefix(launch.args[7], "ssh-ed25519 ") {
		t.Errorf("launch pubkey arg not an authorized_keys line: %q", launch.args[7])
	}
	if !strings.Contains(launch.args[8], "bpftrace") {
		t.Errorf("eBPF packages missing from launch args: %q", launch.args[8])
	}
	if _, err := km.AuthorizedKey(keyLabel("uvm-dk")); err != nil {
		t.Errorf("guest key not stored: %v", err)
	}
}

func TestLaunchCloudImageBootsAndRecords(t *testing.T) {
	fc := &fakeCarrier{}
	m, s, km := testManager(t, fc, okReady)
	runningCarrier(t, s, "carrier", "ubuntu")

	rec, err := m.Launch(context.Background(), Spec{Name: "uvm-test", Carrier: "carrier", Image: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != vault.VMRunning {
		t.Errorf("state = %q, want running", rec.State)
	}

	launch, ok := fc.find(markLaunchCloud)
	if !ok {
		t.Fatal("cloud-image launch script never ran")
	}
	// Cloud-image launch args: dir, url, port, mem, cpus, arch, fw, pub, pkgs.
	if launch.args[0] != handleDir("uvm-test") {
		t.Errorf("launch dir arg = %q", launch.args[0])
	}
	if launch.args[2] != strconv.Itoa(portFor("uvm-test")) {
		t.Errorf("launch port arg = %q", launch.args[2])
	}
	if !strings.HasPrefix(launch.args[7], "ssh-ed25519 ") {
		t.Errorf("launch pubkey arg not an authorized_keys line: %q", launch.args[7])
	}
	if _, err := km.AuthorizedKey(keyLabel("uvm-test")); err != nil {
		t.Errorf("guest key not stored: %v", err)
	}
	if got, err := m.Get("uvm-test"); err != nil || got.State != vault.VMRunning {
		t.Errorf("Get = %+v, %v", got, err)
	}
}

func TestLaunchRejectsBadCarrier(t *testing.T) {
	fc := &fakeCarrier{}
	m, s, _ := testManager(t, fc, okReady)

	// Missing carrier.
	if _, err := m.Launch(context.Background(), Spec{Carrier: "ghost"}); err == nil {
		t.Error("launch with missing carrier succeeded")
	}
	// Stopped carrier.
	if err := s.CreateVM("stopped", "ubuntu"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVMState("stopped", vault.VMStopped); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Launch(context.Background(), Spec{Carrier: "stopped"}); err == nil {
		t.Error("launch with stopped carrier succeeded")
	}
	// Non-debian carrier.
	runningCarrier(t, s, "alpinebox", "alpine")
	if _, err := m.Launch(context.Background(), Spec{Carrier: "alpinebox"}); err == nil {
		t.Error("launch on non-debian carrier succeeded")
	}
}

func TestLaunchTearsDownOnBootFailure(t *testing.T) {
	fc := &fakeCarrier{}
	bootErr := errors.New("guest never came up")
	m, s, km := testManager(t, fc, func(context.Context, string) error { return bootErr })
	runningCarrier(t, s, "carrier", "ubuntu")

	if _, err := m.Launch(context.Background(), Spec{Name: "uvm-x", Carrier: "carrier"}); err == nil {
		t.Fatal("launch succeeded despite boot failure")
	}
	// Record is tombstoned.
	rec, err := m.Get("uvm-x")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != vault.VMDestroyed {
		t.Errorf("state = %q, want destroyed", rec.State)
	}
	// Guest key was revoked.
	if _, err := km.AuthorizedKey(keyLabel("uvm-x")); err == nil {
		t.Error("guest key survived teardown")
	}
	// Teardown ran the destroy script.
	if _, ok := fc.find(markDestroy); !ok {
		t.Error("destroy script never ran during teardown")
	}
}

func TestStopStartDestroy(t *testing.T) {
	fc := &fakeCarrier{}
	m, s, km := testManager(t, fc, okReady)
	runningCarrier(t, s, "carrier", "ubuntu")
	if _, err := m.Launch(context.Background(), Spec{Name: "uvm-l", Carrier: "carrier"}); err != nil {
		t.Fatal(err)
	}

	if err := m.Stop(context.Background(), "uvm-l"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get("uvm-l"); got.State != vault.VMStopped {
		t.Errorf("after stop: state = %q", got.State)
	}
	if _, ok := fc.find(markStop); !ok {
		t.Error("stop script never ran")
	}

	if _, err := m.Start(context.Background(), "uvm-l"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get("uvm-l"); got.State != vault.VMRunning {
		t.Errorf("after start: state = %q", got.State)
	}
	if _, ok := fc.find(markStartDK); !ok {
		t.Error("direct-kernel start script never ran")
	}

	if err := m.Destroy(context.Background(), "uvm-l", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get("uvm-l"); got.State != vault.VMDestroyed {
		t.Errorf("after destroy: state = %q", got.State)
	}
	if _, err := km.AuthorizedKey(keyLabel("uvm-l")); err == nil {
		t.Error("purge_key did not remove the guest key")
	}
}

// stubResolver is the fallback target for the resolver test.
type stubResolver struct{ called bool }

func (s *stubResolver) Resolve(context.Context, string) (exec.ConnInfo, error) {
	s.called = true
	return exec.ConnInfo{Addr: "fallback:22", User: "agent"}, nil
}

func TestResolverMapsMicroVMAndFallsThrough(t *testing.T) {
	s, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	km := keys.NewManager(s)

	keyID, _, err := km.Generate(keyLabel("uvm-r"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateMicroVM(vault.MicroVM{
		ID: "uvm-r", Carrier: "carrier", Image: "ubuntu", KeyID: keyID, SSHPort: 24680,
	}); err != nil {
		t.Fatal(err)
	}

	next := &stubResolver{}
	r := NewResolver(s, km, next)

	info, err := r.Resolve(context.Background(), "uvm-r")
	if err != nil {
		t.Fatal(err)
	}
	if info.Addr != "carrier.orb.local:24680" {
		t.Errorf("addr = %q, want carrier.orb.local:24680", info.Addr)
	}
	if info.User != "agent" || info.Signer == nil {
		t.Errorf("conn info incomplete: %+v", info)
	}
	if next.called {
		t.Error("fallback resolver should not have been called for a microVM id")
	}

	// Unknown id falls through to the next resolver.
	if _, err := r.Resolve(context.Background(), "some-vm"); err != nil {
		t.Fatal(err)
	}
	if !next.called {
		t.Error("fallback resolver was not called for an unknown id")
	}
}
