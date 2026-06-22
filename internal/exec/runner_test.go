package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"orbit/internal/keys"
	"orbit/internal/vault"
)

// testEnv wires a vault, key manager, fake SSH server, and runner with the
// given VMs registered (each with its own keypair, authorized on the server).
func testEnv(t *testing.T, vmIDs ...string) *Runner {
	t.Helper()

	s, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	km := keys.NewManager(s)

	authorized := make(map[string]ssh.PublicKey)
	for _, id := range vmIDs {
		if err := s.CreateVM(id, "ubuntu"); err != nil {
			t.Fatal(err)
		}
		keyID, kp, err := km.Generate("vm-"+id, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetVMKey(id, keyID); err != nil {
			t.Fatal(err)
		}
		if err := s.SetVMState(id, vault.VMRunning); err != nil {
			t.Fatal(err)
		}
		pub, err := ssh.NewPublicKey(kp.Public)
		if err != nil {
			t.Fatal(err)
		}
		authorized["agent@"+id] = pub
	}

	addr := startSSHServer(t, authorized)
	// Route every VM to the test server, keeping per-VM users so tests
	// can tell connections apart (mirrors one-sshd-per-VM in production).
	return NewRunner(s, km, WithTarget(func(vmID string) (string, string) {
		return addr, "agent@" + vmID
	}))
}

func TestExecCapturesStdout(t *testing.T) {
	r := testEnv(t, "vm1")
	res, err := r.Exec(context.Background(), "vm1", "echo hello vault")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hello vault\n" || res.ExitCode != 0 {
		t.Errorf("got %+v", res)
	}
}

func TestExecReportsExitCodeAndStderr(t *testing.T) {
	r := testEnv(t, "vm1")
	res, err := r.Exec(context.Background(), "vm1", "fail")
	if err != nil {
		t.Fatalf("non-zero exit must not be a Go error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if res.Stderr != "went wrong\n" {
		t.Errorf("stderr = %q", res.Stderr)
	}
	if res.Stdout != "" {
		t.Errorf("stdout = %q, want empty", res.Stdout)
	}
}

func TestExecAuthenticatesAsVMUser(t *testing.T) {
	r := testEnv(t, "sandbox-7")
	res, err := r.Exec(context.Background(), "sandbox-7", "whoami")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "agent@sandbox-7\n" {
		t.Errorf("ssh user = %q, want agent@sandbox-7", strings.TrimSpace(res.Stdout))
	}
}

func TestExecContextTimeout(t *testing.T) {
	r := testEnv(t, "vm1")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Exec(ctx, "vm1", "sleep")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout took %v, command did not unblock promptly", elapsed)
	}
}

func TestExecRequiresKnownLiveVM(t *testing.T) {
	r := testEnv(t, "vm1")
	ctx := context.Background()

	if _, err := r.Exec(ctx, "ghost", "echo x"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("unknown vm: err = %v, want ErrNotFound", err)
	}

	// Destroyed VMs are refused before any dial.
	env := testEnv(t, "dead")
	if err := envStore(t, env, "dead"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Exec(ctx, "dead", "echo x"); err == nil || !strings.Contains(err.Error(), "destroyed") {
		t.Errorf("destroyed vm: err = %v", err)
	}
}

// envStore marks a VM destroyed through the runner's own store reference.
func envStore(t *testing.T, r *Runner, vmID string) error {
	t.Helper()
	return r.store.SetVMState(vmID, vault.VMDestroyed)
}

func TestWrongKeyIsRejected(t *testing.T) {
	r := testEnv(t, "vm1")
	// Replace vm1's key after the server learned the original public key.
	if _, _, err := r.keys.Generate("vm-vm1", "vm1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Exec(context.Background(), "vm1", "echo x"); err == nil {
		t.Error("server accepted a rotated-out key")
	}
}

func TestPushAndPull(t *testing.T) {
	r := testEnv(t, "vm1")
	ctx := context.Background()
	dir := t.TempDir()

	// Push: local bytes -> "remote" path (test server shares our fs).
	remote := filepath.Join(dir, "nested", "payload.bin")
	want := []byte("file ops payload \x00\x01\x02")
	if err := r.PushBytes(ctx, "vm1", want, remote, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(remote)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}

	got, err := r.Pull(ctx, "vm1", remote)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("pull = %q, want %q", got, want)
	}

	// Push from a local file path too.
	local := filepath.Join(dir, "local.txt")
	if err := os.WriteFile(local, []byte("from disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote2 := filepath.Join(dir, "copied.txt")
	if err := r.Push(ctx, "vm1", local, remote2); err != nil {
		t.Fatal(err)
	}
	got2, err := r.Pull(ctx, "vm1", remote2)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != "from disk" {
		t.Errorf("pull = %q", got2)
	}

	if _, err := r.Pull(ctx, "vm1", filepath.Join(dir, "missing")); err == nil {
		t.Error("pulling a missing file succeeded")
	}
}

func TestExecScriptUploadsRunsAndCleans(t *testing.T) {
	r := testEnv(t, "vm1")
	res, err := r.ExecScript(context.Background(), "vm1", "#!/bin/sh\necho from script\n", "sh")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "echo from script") {
		t.Errorf("got %+v", res)
	}

	// The temp script must be removed afterwards.
	matches, _ := filepath.Glob("/tmp/orbit-*.script")
	if len(matches) != 0 {
		t.Errorf("leftover script files: %v", matches)
	}
}

func TestExecParallel(t *testing.T) {
	r := testEnv(t, "vm-a", "vm-b", "vm-c")
	results := r.ExecParallel(context.Background(), []string{"vm-a", "vm-b", "vm-c", "ghost"}, "whoami", 2)

	if len(results) != 4 {
		t.Fatalf("got %d results", len(results))
	}
	for i, want := range []string{"vm-a", "vm-b", "vm-c"} {
		if results[i].VMID != want {
			t.Errorf("results out of order: %+v", results)
		}
		if results[i].Err != "" {
			t.Errorf("%s failed: %s", want, results[i].Err)
		}
		if got := strings.TrimSpace(results[i].Result.Stdout); got != "agent@"+want {
			t.Errorf("%s stdout = %q", want, got)
		}
	}
	if results[3].Err == "" {
		t.Error("ghost VM should have reported an error")
	}
}
