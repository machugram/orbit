// Package keys implements Ed25519 SSH keypair generation and vault-backed
// storage for Orbit (ORBIT_SPEC.md §1.3).
package keys

import (
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Keypair is an Ed25519 SSH keypair held in memory. The private key never
// touches disk unencrypted; it is persisted only via the vault.
type Keypair struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
	Comment string
}

// PrivatePEM serializes the private key in OpenSSH PEM format, the format
// expected by ssh clients and golang.org/x/crypto/ssh.
func (k *Keypair) PrivatePEM() ([]byte, error) {
	block, err := ssh.MarshalPrivateKey(k.Private, k.Comment)
	if err != nil {
		return nil, fmt.Errorf("keys: marshaling private key: %w", err)
	}
	return pem.EncodeToMemory(block), nil
}

// Signer returns an ssh.Signer for the keypair, for SSH client auth. The
// private key stays in memory; callers should let it go out of scope when the
// connection closes.
func (k *Keypair) Signer() (ssh.Signer, error) {
	signer, err := ssh.NewSignerFromKey(k.Private)
	if err != nil {
		return nil, fmt.Errorf("keys: building signer: %w", err)
	}
	return signer, nil
}

// AuthorizedKey returns the public key as a single authorized_keys line:
// "ssh-ed25519 AAAA... comment". Safe to share and to inject via cloud-init.
func (k *Keypair) AuthorizedKey() (string, error) {
	pub, err := ssh.NewPublicKey(k.Public)
	if err != nil {
		return "", fmt.Errorf("keys: encoding public key: %w", err)
	}
	line := strings.TrimRight(string(ssh.MarshalAuthorizedKey(pub)), "\n")
	if k.Comment != "" {
		line += " " + k.Comment
	}
	return line, nil
}

// parsePEM reverses PrivatePEM.
func parsePEM(pemBytes []byte, comment string) (*Keypair, error) {
	raw, err := ssh.ParseRawPrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("keys: parsing private key: %w", err)
	}
	var priv ed25519.PrivateKey
	switch p := raw.(type) {
	case ed25519.PrivateKey:
		priv = p
	case *ed25519.PrivateKey:
		priv = *p
	default:
		return nil, fmt.Errorf("keys: expected Ed25519 key, got %T", raw)
	}
	return &Keypair{
		Private: priv,
		Public:  priv.Public().(ed25519.PublicKey),
		Comment: comment,
	}, nil
}
