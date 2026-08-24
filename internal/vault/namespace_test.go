package vault_test

import (
	"path/filepath"
	"testing"

	"github.com/machugram/orbit/internal/vault"
)

func TestAgentNamespaceIsolatesVMs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.db")
	pass := []byte("pw")

	a, err := vault.Open(path, pass, vault.WithAgentNamespace("alice"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.CreateVM("alice-vm", "alpine"); err != nil {
		t.Fatal(err)
	}
	_ = a.SetVMState("alice-vm", vault.VMRunning)

	b, err := vault.Open(path, pass, vault.WithAgentNamespace("bob"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, err := b.GetVM("alice-vm"); err != vault.ErrNotFound {
		t.Fatalf("bob GetVM = %v, want ErrNotFound", err)
	}
	list, err := b.ListVMs()
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range list {
		if vm.ID == "alice-vm" {
			t.Fatal("bob ListVMs saw alice-vm")
		}
	}

	if err := b.CreateVM("bob-vm", "alpine"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetVM("bob-vm"); err != vault.ErrNotFound {
		t.Fatalf("alice GetVM bob = %v", err)
	}
}

func TestSharedBaseVisibleToAllAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.db")
	pass := []byte("pw")

	a, err := vault.Open(path, pass, vault.WithAgentNamespace("alice"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.CreateVM("orbit-base-alpine", "alpine"); err != nil {
		t.Fatal(err)
	}
	_ = a.SetVMState("orbit-base-alpine", vault.VMRunning)

	b, err := vault.Open(path, pass, vault.WithAgentNamespace("bob"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	rec, err := b.GetVM("orbit-base-alpine")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Namespace != vault.SharedNamespace {
		t.Fatalf("namespace = %q", rec.Namespace)
	}
}

func TestEmptyCredNamespaceUsesAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.db")
	a, err := vault.Open(path, []byte("pw"), vault.WithAgentNamespace("alice"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.SetCredential("", "token", []byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	val, err := a.GetCredential("alice", "token")
	if err != nil {
		t.Fatal(err)
	}
	if string(val) != "s3cret" {
		t.Fatalf("value = %q", val)
	}
	b, err := vault.Open(path, []byte("pw"), vault.WithAgentNamespace("bob"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.GetCredential("", "token"); err != vault.ErrNotFound {
		t.Fatalf("bob should not see alice default cred: %v", err)
	}
}
