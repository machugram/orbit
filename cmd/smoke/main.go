// Command smoke runs Orbit's end-to-end smoke test against the real
// OrbStack installation (spec §11 session 8): it creates a throwaway vault
// and an isolated VM with a freshly generated key, executes commands,
// pushes/pulls files, runs a script, rotates the key, and destroys the VM.
//
// Usage: go run ./cmd/smoke [-distro ubuntu] [-keep]
//
// It creates (and unless -keep, destroys) one real OrbStack machine named
// smoke-<hex>.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/vault"
	"github.com/machugram/orbit/internal/vm"
)

func main() {
	distro := flag.String("distro", "ubuntu", "distro to test with")
	keep := flag.Bool("keep", false, "keep the VM and vault after the test")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall deadline")
	flag.Parse()

	if _, err := osexec.LookPath("orbctl"); err != nil {
		log.Fatal("smoke: orbctl not found in PATH — is OrbStack installed?")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := run(ctx, *distro, *keep); err != nil {
		log.Fatalf("SMOKE FAIL: %v", err)
	}
	fmt.Println("SMOKE PASS")
}

func run(ctx context.Context, distro string, keep bool) error {
	step := func(format string, a ...any) {
		fmt.Printf("==> "+format+"\n", a...)
	}

	dir, err := os.MkdirTemp("", "orbit-smoke-*")
	if err != nil {
		return err
	}
	if !keep {
		defer os.RemoveAll(dir)
	}

	step("opening throwaway vault in %s", dir)
	store, err := vault.Open(filepath.Join(dir, "vault.db"), []byte("smoke-"+vmName()))
	if err != nil {
		return err
	}
	defer store.Close()

	km := keys.NewManager(store)
	vmm := vm.NewManager(store)
	prov := vm.NewProvisioner(km, vmm)
	runner := exec.NewRunner(store, km)

	name := "smoke-" + vmName()
	step("creating isolated %s VM %q with a fresh Ed25519 key (this is the slow part)", distro, name)
	rec, err := prov.Create(ctx, distro, name, true)
	if err != nil {
		return fmt.Errorf("vm_create: %w", err)
	}
	if !keep {
		defer func() {
			step("destroying %s and purging its key", name)
			dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer dcancel()
			if err := vmm.Destroy(dctx, name, true); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: destroy failed, clean up with: orbctl delete -f %s (%v)\n", name, err)
			}
		}()
	}
	step("VM %s is %s (key %s)", rec.ID, rec.State, rec.KeyID)

	// The VM can report running before sshd accepts connections.
	step("exec: uname -a && id (retrying until sshd accepts the key)")
	res, err := retryExec(ctx, runner, name, "uname -a && id")
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exec: exit %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	fmt.Print(res.Stdout)
	if !strings.Contains(res.Stdout, "agent") {
		return fmt.Errorf("exec: expected to run as the agent user, got: %s", res.Stdout)
	}

	step("file_push + exec + file_pull roundtrip")
	payload := []byte("orbit smoke payload " + vmName())
	if err := runner.PushBytes(ctx, name, payload, "/tmp/smoke/payload.txt", 0o644); err != nil {
		return fmt.Errorf("file_push: %w", err)
	}
	res, err = runner.Exec(ctx, name, "cat /tmp/smoke/payload.txt")
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("exec cat: %v (res %+v)", err, res)
	}
	if res.Stdout != string(payload) {
		return fmt.Errorf("payload mismatch: %q", res.Stdout)
	}
	pulled, err := runner.Pull(ctx, name, "/tmp/smoke/payload.txt")
	if err != nil {
		return fmt.Errorf("file_pull: %w", err)
	}
	if string(pulled) != string(payload) {
		return fmt.Errorf("file_pull mismatch: %q", pulled)
	}

	step("exec_script via upload")
	res, err = runner.ExecScript(ctx, name, "#!/bin/sh\necho script-ran-on $(hostname)\n", "sh")
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("exec_script: %v (res %+v)", err, res)
	}
	fmt.Print(res.Stdout)

	step("non-zero exit codes are reported, not errors")
	res, err = runner.Exec(ctx, name, "exit 42")
	if err != nil {
		return fmt.Errorf("exec exit 42: %w", err)
	}
	if res.ExitCode != 42 {
		return fmt.Errorf("exit code = %d, want 42", res.ExitCode)
	}

	step("key rotation: replace authorized_keys with a new keypair")
	oldKP, err := km.Load(vm.KeyLabel(name))
	if err != nil {
		return err
	}
	newKP, err := keys.Generate(vm.KeyLabel(name))
	if err != nil {
		return err
	}
	newPub, err := newKP.AuthorizedKey()
	if err != nil {
		return err
	}
	home, err := runner.Exec(ctx, name, `printf %s "$HOME"`)
	if err != nil || home.ExitCode != 0 {
		return fmt.Errorf("resolving home: %v (res %+v)", err, home)
	}
	authKeys := strings.TrimSpace(home.Stdout) + "/.ssh/authorized_keys"
	if err := runner.PushBytes(ctx, name, []byte(strings.TrimSpace(newPub)+"\n"), authKeys, 0o600); err != nil {
		return fmt.Errorf("rotating authorized_keys: %w", err)
	}
	if _, err := km.Put(vm.KeyLabel(name), name, newKP); err != nil {
		return err
	}
	// Drop the connection that authenticated with the old key. The next
	// dial has to present the key just written to the vault.
	runner.Invalidate(name)
	res, err = retryExec(ctx, runner, name, "echo rotated-key-works")
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("exec with rotated key: %v (res %+v)", err, res)
	}
	_ = oldKP // old key is now rotated out of the vault

	if keep {
		step("keeping VM %s and vault %s (-keep)", name, dir)
	}
	return nil
}

// commandRunner is the Exec method the retry loop needs. *exec.Runner
// implements it; tests pass a fake.
type commandRunner interface {
	Exec(ctx context.Context, vmID, command string) (*exec.Result, error)
}

// retryExec retries to absorb the gap between "VM running" and sshd
// accepting connections. The window is 4 minutes, the pause 3 seconds.
func retryExec(ctx context.Context, r commandRunner, vmID, cmd string) (*exec.Result, error) {
	return retryExecFor(ctx, r, vmID, cmd, 4*time.Minute, 3*time.Second)
}

func retryExecFor(ctx context.Context, r commandRunner, vmID, cmd string, window, pause time.Duration) (*exec.Result, error) {
	deadline := time.Now().Add(window)
	var res *exec.Result
	var err error
	for {
		res, err = r.Exec(ctx, vmID, cmd)
		if err == nil {
			return res, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return res, cerr
		}
		if time.Now().After(deadline) {
			return res, err
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return res, ctx.Err()
		case <-timer.C:
		}
	}
}

func vmName() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
