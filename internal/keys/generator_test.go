package keys

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/machugram/orbit/internal/vault"
)

func TestGenerateProducesWorkingKeypair(t *testing.T) {
	kp, err := Generate("test-key")
	if err != nil {
		t.Fatal(err)
	}

	msg := []byte("orbit signature check")
	sig := ed25519.Sign(kp.Private, msg)
	if !ed25519.Verify(kp.Public, msg, sig) {
		t.Error("signature from generated private key does not verify with public key")
	}
}

func TestGenerateIsUnique(t *testing.T) {
	a, err := Generate("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate("b")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Private, b.Private) {
		t.Error("two generated keypairs are identical")
	}
}

func TestAuthorizedKeyFormat(t *testing.T) {
	kp, err := Generate("vm-sandbox-123")
	if err != nil {
		t.Fatal(err)
	}
	line, err := kp.AuthorizedKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "ssh-ed25519 ") {
		t.Errorf("authorized key line = %q, want ssh-ed25519 prefix", line)
	}
	if !strings.HasSuffix(line, " vm-sandbox-123") {
		t.Errorf("authorized key line missing comment: %q", line)
	}

	// The line must parse with the ssh package (what sshd does).
	parsed, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatalf("ParseAuthorizedKey: %v", err)
	}
	if parsed.Type() != "ssh-ed25519" {
		t.Errorf("parsed type = %q", parsed.Type())
	}
	if comment != "vm-sandbox-123" {
		t.Errorf("parsed comment = %q", comment)
	}
}

func TestPrivatePEMRoundtrip(t *testing.T) {
	kp, err := Generate("roundtrip")
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, err := kp.PrivatePEM()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pemBytes, []byte("OPENSSH PRIVATE KEY")) {
		t.Errorf("PEM does not look like OpenSSH format:\n%s", pemBytes)
	}

	got, err := parsePEM(pemBytes, "roundtrip")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Private, kp.Private) {
		t.Error("private key changed across PEM roundtrip")
	}
	if !bytes.Equal(got.Public, kp.Public) {
		t.Error("public key changed across PEM roundtrip")
	}
}

func openTestVault(t *testing.T, path string) *vault.Store {
	t.Helper()
	s, err := vault.Open(path, []byte("test-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestManagerStoreAndLoad(t *testing.T) {
	s := openTestVault(t, filepath.Join(t.TempDir(), "vault.db"))
	m := NewManager(s)

	id, kp, err := m.Generate("vm-sandbox-key", "sandbox-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("empty credential id")
	}

	loaded, err := m.Load("vm-sandbox-key")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Private, kp.Private) {
		t.Error("loaded private key differs from generated one")
	}

	wantPub, _ := kp.AuthorizedKey()
	gotPub, err := m.AuthorizedKey("vm-sandbox-key")
	if err != nil {
		t.Fatal(err)
	}
	if gotPub != wantPub {
		t.Errorf("AuthorizedKey = %q, want %q", gotPub, wantPub)
	}

	infos, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("got %d keys, want 1", len(infos))
	}
	if infos[0].ID != id || infos[0].VMID != "sandbox-xyz" || infos[0].Namespace != Namespace {
		t.Errorf("unexpected listing: %+v", infos[0])
	}
}

func TestManagerRotateInPlace(t *testing.T) {
	s := openTestVault(t, filepath.Join(t.TempDir(), "vault.db"))
	m := NewManager(s)

	id1, kp1, err := m.Generate("rotating-key", "")
	if err != nil {
		t.Fatal(err)
	}
	id2, kp2, err := m.Generate("rotating-key", "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("rotation changed credential id: %s -> %s", id1, id2)
	}
	if bytes.Equal(kp1.Private, kp2.Private) {
		t.Error("rotation did not produce a new key")
	}

	loaded, err := m.Load("rotating-key")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Private, kp2.Private) {
		t.Error("vault did not return the rotated key")
	}

	infos, _ := m.List()
	if len(infos) != 1 || infos[0].RotatedAt == 0 {
		t.Errorf("expected one key with rotated_at set, got %+v", infos)
	}
}

func TestManagerRevoke(t *testing.T) {
	s := openTestVault(t, filepath.Join(t.TempDir(), "vault.db"))
	m := NewManager(s)

	if _, _, err := m.Generate("doomed", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke("doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Load("doomed"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("load after revoke: err = %v, want ErrNotFound", err)
	}
	if err := m.Revoke("doomed"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("double revoke: err = %v, want ErrNotFound", err)
	}
}

func TestKeySurvivesVaultReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.db")

	s, err := vault.Open(path, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	_, kp, err := NewManager(s).Generate("persistent-key", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := vault.Open(path, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	loaded, err := NewManager(s2).Load("persistent-key")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Private, kp.Private) {
		t.Error("key changed across vault reopen")
	}
}

func TestKeysAreNamespaced(t *testing.T) {
	s := openTestVault(t, filepath.Join(t.TempDir(), "vault.db"))
	m := NewManager(s)

	if _, _, err := m.Generate("my-key", ""); err != nil {
		t.Fatal(err)
	}

	// A credential in the default namespace must not collide or leak.
	if _, err := s.GetCredential("default", "my-key"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("key visible outside %q namespace: err = %v", Namespace, err)
	}
}
