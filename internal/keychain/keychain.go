// Package keychain resolves the vault master passphrase. Order: the
// ORBIT_PASSPHRASE environment variable, then the macOS Keychain
// (via the security CLI — the keytar role in spec §3). On first use a
// random passphrase is generated and stored in the Keychain, so the vault
// works out of the box with no prompting.
package keychain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	osexec "os/exec"
	"runtime"
	"strings"
)

const service = "orbit-vault"

// store abstracts the OS secret backend so the resolution logic can be tested
// without shelling out to the real `security` binary. The default
// implementation talks to the macOS Keychain.
type store interface {
	// lookup returns the stored passphrase, or an error if none exists.
	lookup(account string) (string, error)
	// save persists a passphrase for account.
	save(account, pass string) error
}

// Passphrase returns the vault master passphrase, creating and persisting
// one in the macOS Keychain on first use.
func Passphrase() ([]byte, error) {
	return resolve(os.Getenv("ORBIT_PASSPHRASE"), runtime.GOOS, os.Getenv("USER"), keychainStore{}, randomPassphrase)
}

// resolve is the testable core: env var wins, then the OS store, then a
// freshly generated passphrase persisted for next time. Keeping the policy
// here (separate from the macOS-specific store) lets tests exercise every
// branch on any platform.
func resolve(env, goos, account string, s store, gen func() (string, error)) ([]byte, error) {
	if env != "" {
		return []byte(env), nil
	}
	if goos != "darwin" {
		return nil, fmt.Errorf("keychain: set ORBIT_PASSPHRASE (Keychain storage is macOS-only)")
	}

	if pass, err := s.lookup(account); err == nil {
		return []byte(pass), nil
	}

	// First run: generate and persist a random passphrase.
	pass, err := gen()
	if err != nil {
		return nil, fmt.Errorf("keychain: generating passphrase: %w", err)
	}
	if err := s.save(account, pass); err != nil {
		return nil, fmt.Errorf("keychain: storing passphrase: %w", err)
	}
	return []byte(pass), nil
}

// randomPassphrase returns a 32-byte passphrase as a 64-char hex string.
func randomPassphrase() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// keychainStore is the production store backed by the macOS `security` CLI.
type keychainStore struct{}

func (keychainStore) lookup(account string) (string, error) {
	out, err := osexec.Command("security", "find-generic-password",
		"-s", service, "-a", account, "-w").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (keychainStore) save(account, pass string) error {
	// Pass the secret on stdin, never in argv: `security add-generic-password
	// -w` (with no value) reads the password from stdin, prompting for it
	// twice (entry + confirmation). Supplying it as `-w <pass>` would leave the
	// vault master passphrase visible in `ps`/`/proc` for any local process.
	cmd := osexec.Command("security", "add-generic-password",
		"-s", service, "-a", account, "-U", "-w")
	cmd.Stdin = strings.NewReader(pass + "\n" + pass + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
