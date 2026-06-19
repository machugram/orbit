package vault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vault.db")
	s, err := Open(path, []byte("test-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestCredentialRoundtrip(t *testing.T) {
	s, _ := openTestStore(t)

	secret := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----")
	id, err := s.SetCredential("default", "vm-sandbox-key", secret, WithLabel("sandbox keypair"))
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("SetCredential returned empty id")
	}

	got, err := s.GetCredential("default", "vm-sandbox-key")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, secret) {
		t.Error("retrieved value does not match stored value")
	}
}

func TestUpsertKeepsIDAndSetsRotatedAt(t *testing.T) {
	s, _ := openTestStore(t)

	id1, err := s.SetCredential("", "api-token", []byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.SetCredential("", "api-token", []byte("v2"))
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("upsert changed credential id: %s -> %s", id1, id2)
	}

	got, err := s.GetCredential("", "api-token")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Errorf("value = %q, want %q", got, "v2")
	}

	infos, err := s.ListCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("got %d credentials, want 1", len(infos))
	}
	if infos[0].RotatedAt == 0 {
		t.Error("rotated_at not set after overwrite")
	}
}

func TestNamespaceIsolation(t *testing.T) {
	s, _ := openTestStore(t)

	if _, err := s.SetCredential("prod", "token", []byte("prod-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetCredential("dev", "token", []byte("dev-secret")); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetCredential("prod", "token")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "prod-secret" {
		t.Errorf("prod value = %q", got)
	}

	if _, err := s.GetCredential("default", "token"); !errors.Is(err, ErrNotFound) {
		t.Errorf("default namespace leaked: err = %v, want ErrNotFound", err)
	}

	infos, err := s.ListCredentials("dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Key != "token" || infos[0].Namespace != "dev" {
		t.Errorf("unexpected dev listing: %+v", infos)
	}
}

func TestDeleteCredential(t *testing.T) {
	s, _ := openTestStore(t)

	if _, err := s.SetCredential("", "doomed", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCredential("", "doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCredential("", "doomed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteCredential("", "doomed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: err = %v, want ErrNotFound", err)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.db")

	s, err := Open(path, []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetCredential("", "persisted", []byte("survives reopen")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.GetCredential("", "persisted")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "survives reopen" {
		t.Errorf("value = %q", got)
	}
}

func TestWrongPassphraseRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.db")

	s, err := Open(path, []byte("right"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := Open(path, []byte("wrong")); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("err = %v, want ErrWrongPassphrase", err)
	}
}

func TestSecretsNeverPlaintextOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.db")

	s, err := Open(path, []byte("passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("TOP-SECRET-MARKER-9f8e7d6c")
	if _, err := s.SetCredential("", "marker", secret); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Close checkpoints WAL; scan every file SQLite may have left behind.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		raw, err := os.ReadFile(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, secret) {
			t.Errorf("plaintext secret found in %s", path+suffix)
		}
	}
}

func TestVMLifecycle(t *testing.T) {
	s, _ := openTestStore(t)

	if err := s.CreateVM("sandbox-xyz", "ubuntu"); err != nil {
		t.Fatal(err)
	}

	vm, err := s.GetVM("sandbox-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != VMCreating || vm.Distro != "ubuntu" || vm.CreatedAt == 0 {
		t.Errorf("unexpected vm record: %+v", vm)
	}

	keyID, err := s.SetCredential("ssh-keys", "vm-sandbox-xyz", []byte("privkey"), WithVM("sandbox-xyz"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetVMKey("sandbox-xyz", keyID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVMState("sandbox-xyz", VMRunning); err != nil {
		t.Fatal(err)
	}

	vm, err = s.GetVM("sandbox-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if vm.State != VMRunning || vm.KeyID != keyID {
		t.Errorf("after update: %+v", vm)
	}

	if err := s.SetVMState("sandbox-xyz", VMDestroyed); err != nil {
		t.Fatal(err)
	}
	vm, _ = s.GetVM("sandbox-xyz")
	if vm.State != VMDestroyed || vm.DestroyedAt == 0 {
		t.Errorf("after destroy: %+v", vm)
	}

	vms, err := s.ListVMs()
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 {
		t.Fatalf("got %d vms, want 1", len(vms))
	}

	if _, err := s.GetVM("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing vm: err = %v, want ErrNotFound", err)
	}
	if err := s.SetVMState("nope", VMRunning); !errors.Is(err, ErrNotFound) {
		t.Errorf("state on missing vm: err = %v, want ErrNotFound", err)
	}
}
