// Package vault implements Orbit's encrypted credential store:
// Argon2id key derivation, AES-256-GCM encryption, and a SQLite backend.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

const (
	// SaltSize is the Argon2id salt length in bytes.
	SaltSize = 32
	// KeySize is the derived master key length in bytes (AES-256).
	KeySize = 32
	// NonceSize is the AES-GCM nonce (IV) length in bytes.
	NonceSize = 12
	// TagSize is the AES-GCM authentication tag length in bytes.
	TagSize = 16

	// Argon2id parameters per ORBIT_SPEC.md §3:
	// m=65536 KiB, t=3, p=4.
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 4
)

var (
	// ErrDecrypt is returned when decryption fails, either because the
	// master key is wrong or the ciphertext was tampered with. GCM cannot
	// distinguish the two cases.
	ErrDecrypt = errors.New("vault: decryption failed (wrong key or corrupted data)")
)

// NewSalt returns a fresh random Argon2id salt.
func NewSalt() ([]byte, error) {
	salt := make([]byte, SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("vault: generating salt: %w", err)
	}
	return salt, nil
}

// DeriveKey derives a 32-byte master key from a passphrase and salt using
// Argon2id. The same passphrase and salt always yield the same key.
func DeriveKey(passphrase, salt []byte) []byte {
	return argon2.IDKey(passphrase, salt, argonTime, argonMemory, argonThreads, KeySize)
}

// Encrypt encrypts plaintext with AES-256-GCM under key, using a fresh
// random nonce. It returns the ciphertext, nonce, and authentication tag
// separately to match the vault schema (ciphertext / iv / tag columns).
func Encrypt(key, plaintext []byte) (ciphertext, nonce, tag []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, nil, err
	}

	nonce = make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, nil, fmt.Errorf("vault: generating nonce: %w", err)
	}

	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	// Seal appends the tag to the ciphertext; split them for storage.
	ciphertext = sealed[:len(sealed)-TagSize]
	tag = sealed[len(sealed)-TagSize:]
	return ciphertext, nonce, tag, nil
}

// Decrypt reverses Encrypt. It returns ErrDecrypt if the key is wrong or
// any of ciphertext, nonce, or tag has been tampered with.
func Decrypt(key, ciphertext, nonce, tag []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != NonceSize {
		return nil, ErrDecrypt
	}

	sealed := make([]byte, 0, len(ciphertext)+len(tag))
	sealed = append(sealed, ciphertext...)
	sealed = append(sealed, tag...)

	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("vault: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: creating GCM: %w", err)
	}
	return gcm, nil
}
