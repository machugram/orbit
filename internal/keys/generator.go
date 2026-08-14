package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/ssh"

	"github.com/machugram/orbit/internal/vault"
)

// Namespace is the vault namespace holding all SSH private keys.
const Namespace = "ssh-keys"

// Generate creates a new Ed25519 keypair in memory. It does not persist
// anything; use Manager.Generate to store the key in the vault.
func Generate(comment string) (*Keypair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("keys: generating keypair: %w", err)
	}
	return &Keypair{Private: priv, Public: pub, Comment: comment}, nil
}

// Manager stores and retrieves SSH keypairs through the encrypted vault.
// Keys are addressed by label within the "ssh-keys" namespace. The public
// key is derived from the private key on demand rather than stored
// separately — an Ed25519 private key embeds its public half, and the vault
// is always unlocked when the manager is in use.
type Manager struct {
	store *vault.Store
}

// NewManager returns a Manager backed by an open vault.
func NewManager(s *vault.Store) *Manager {
	return &Manager{store: s}
}

// Generate creates a keypair, encrypts the private key into the vault under
// label, and returns the vault credential ID alongside the keypair. vmID
// optionally associates the key with a VM record (pass "" for none).
// Generating under an existing label rotates it in place: the credential ID
// is kept and rotated_at is set.
func (m *Manager) Generate(label, vmID string) (string, *Keypair, error) {
	kp, err := Generate(label)
	if err != nil {
		return "", nil, err
	}
	id, err := m.Put(label, vmID, kp)
	if err != nil {
		return "", nil, err
	}
	return id, kp, nil
}

// Put stores an already-generated keypair under label, rotating in place if
// the label exists. Used by key_rotate, where the new key must be pushed to
// the VM before it replaces the old one in the vault.
func (m *Manager) Put(label, vmID string, kp *Keypair) (string, error) {
	pemBytes, err := kp.PrivatePEM()
	if err != nil {
		return "", err
	}
	opts := []vault.SetOption{vault.WithLabel(label)}
	if vmID != "" {
		opts = append(opts, vault.WithVM(vmID))
	}
	id, err := m.store.SetCredential(Namespace, label, pemBytes, opts...)
	if err != nil {
		return "", fmt.Errorf("keys: storing private key: %w", err)
	}
	return id, nil
}

// SignerByID loads the keypair for a vault credential ID and returns an
// ssh.Signer for it. Used by the exec runner and microvm resolver so the
// key→signer step lives in one place.
func (m *Manager) SignerByID(id string) (ssh.Signer, error) {
	kp, err := m.LoadByID(id)
	if err != nil {
		return nil, err
	}
	return kp.Signer()
}

// LoadByID decrypts the keypair stored under a vault credential ID (the
// key_id referenced by a VM record).
func (m *Manager) LoadByID(id string) (*Keypair, error) {
	pemBytes, info, err := m.store.GetCredentialByID(id)
	if err != nil {
		return nil, fmt.Errorf("keys: loading credential %s: %w", id, err)
	}
	return parsePEM(pemBytes, info.Key)
}

// Load decrypts and returns the keypair stored under label.
func (m *Manager) Load(label string) (*Keypair, error) {
	pemBytes, err := m.store.GetCredential(Namespace, label)
	if err != nil {
		return nil, fmt.Errorf("keys: loading %q: %w", label, err)
	}
	return parsePEM(pemBytes, label)
}

// AuthorizedKey returns the public half of the key stored under label as an
// authorized_keys line (the key_export_public MCP tool).
func (m *Manager) AuthorizedKey(label string) (string, error) {
	kp, err := m.Load(label)
	if err != nil {
		return "", err
	}
	return kp.AuthorizedKey()
}

// List returns metadata for all stored keys — never key material.
func (m *Manager) List() ([]vault.CredentialInfo, error) {
	return m.store.ListCredentials(Namespace)
}

// Revoke deletes the key stored under label from the vault. Removing the
// public key from a VM's authorized_keys is the VM manager's job (Session 3+).
func (m *Manager) Revoke(label string) error {
	if err := m.store.DeleteCredential(Namespace, label); err != nil {
		return fmt.Errorf("keys: revoking %q: %w", label, err)
	}
	return nil
}
