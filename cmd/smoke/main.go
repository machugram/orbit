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

	// cloud-init may still be finishing when the VM reports running; SSH
	// retries paper over the gap.
	step("exec: uname -a && id (retrying while cloud-init settles)")
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
	newPub, _ := newKP.AuthorizedKey()
	res, err = runner.Exec(ctx, name,
		fmt.Sprintf(`printf '%%s\n' '%s' > ~/.ssh/authorized_keys`, newPub))
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("rotating authorized_keys: %v (res %+v)", err, res)
	}
	if _, err := km.Put(vm.KeyLabel(name), name, newKP); err != nil {
		return err
	}
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

// retryExec retries to absorb cloud-init lag: sshd only becomes reachable
// after cloud-init's package phase installs openssh-server.
func retryExec(ctx context.Context, r *exec.Runner, vmID, cmd string) (*exec.Result, error) {
	deadline := time.Now().Add(4 * time.Minute)
	for {
		res, err := r.Exec(ctx, vmID, cmd)
		if err == nil {
			return res, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return res, err
		}
		time.Sleep(3 * time.Second)
	}
}

func vmName() string {
	b := make([]byte, 4)
	if f, err := os.Open("/dev/urandom"); err == nil {
		f.Read(b)
		f.Close()
	}
	return fmt.Sprintf("%x", b)
}
