package keychain

import (
	"errors"
	"testing"
)

// fakeStore is an in-memory secret backend for testing the resolve policy.
type fakeStore struct {
	stored    map[string]string
	lookupErr error // forced lookup failure (simulates "no item yet")
	saveErr   error // forced save failure
	saves     int
}

func newFakeStore() *fakeStore { return &fakeStore{stored: map[string]string{}} }

func (f *fakeStore) lookup(account string) (string, error) {
	if f.lookupErr != nil {
		return "", f.lookupErr
	}
	v, ok := f.stored[account]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (f *fakeStore) save(account, pass string) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.stored[account] = pass
	f.saves++
	return nil
}

func constGen(s string) func() (string, error) {
	return func() (string, error) { return s, nil }
}

func TestEnvVarTakesPriority(t *testing.T) {
	s := newFakeStore()
	s.stored["alice"] = "from-keychain"
	got, err := resolve("from-env", "darwin", "alice", s, constGen("generated"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-env" {
		t.Errorf("got %q, want the env var value", got)
	}
	if s.saves != 0 {
		t.Error("env path must not touch the keychain")
	}
}

func TestNonDarwinWithoutEnvIsAClearError(t *testing.T) {
	_, err := resolve("", "linux", "alice", newFakeStore(), constGen("x"))
	if err == nil {
		t.Fatal("expected an error on non-darwin without ORBIT_PASSPHRASE")
	}
	// The message must point the user at the override rather than failing
	// cryptically — this is the documented cross-platform escape hatch.
	if !contains(err.Error(), "ORBIT_PASSPHRASE") {
		t.Errorf("error %q should mention ORBIT_PASSPHRASE", err)
	}
}

func TestExistingKeychainPassphraseIsReturned(t *testing.T) {
	s := newFakeStore()
	s.stored["alice"] = "already-here"
	got, err := resolve("", "darwin", "alice", s, constGen("should-not-be-used"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "already-here" {
		t.Errorf("got %q, want the stored passphrase", got)
	}
	if s.saves != 0 {
		t.Error("an existing passphrase must not be overwritten")
	}
}

func TestFirstRunGeneratesAndPersists(t *testing.T) {
	s := newFakeStore() // empty: lookup fails
	got, err := resolve("", "darwin", "alice", s, constGen("freshly-generated"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "freshly-generated" {
		t.Errorf("got %q, want the generated passphrase", got)
	}
	if s.saves != 1 {
		t.Errorf("generated passphrase should be persisted exactly once, got %d saves", s.saves)
	}
	if s.stored["alice"] != "freshly-generated" {
		t.Error("generated passphrase was not stored for next startup")
	}
}

func TestSaveFailureSurfaces(t *testing.T) {
	s := newFakeStore()
	s.saveErr = errors.New("keychain locked")
	_, err := resolve("", "darwin", "alice", s, constGen("x"))
	if err == nil {
		t.Fatal("expected save failure to surface")
	}
}

func TestRandomPassphraseIsHex64(t *testing.T) {
	p, err := randomPassphrase()
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 64 {
		t.Errorf("passphrase length = %d, want 64 hex chars (32 bytes)", len(p))
	}
	// Two draws must differ — confirms it is actually random, not constant.
	p2, _ := randomPassphrase()
	if p == p2 {
		t.Error("two generated passphrases were identical")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
