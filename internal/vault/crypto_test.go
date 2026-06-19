package vault

import (
	"bytes"
	"errors"
	"testing"
)

func TestDeriveKeyDeterministic(t *testing.T) {
	salt, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	if len(salt) != SaltSize {
		t.Fatalf("salt length = %d, want %d", len(salt), SaltSize)
	}

	k1 := DeriveKey([]byte("correct horse battery staple"), salt)
	k2 := DeriveKey([]byte("correct horse battery staple"), salt)
	if len(k1) != KeySize {
		t.Fatalf("key length = %d, want %d", len(k1), KeySize)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("same passphrase and salt produced different keys")
	}
}

func TestDeriveKeyVariesWithInputs(t *testing.T) {
	salt1, _ := NewSalt()
	salt2, _ := NewSalt()
	pass := []byte("passphrase")

	if bytes.Equal(DeriveKey(pass, salt1), DeriveKey(pass, salt2)) {
		t.Error("different salts produced the same key")
	}
	if bytes.Equal(DeriveKey(pass, salt1), DeriveKey([]byte("other"), salt1)) {
		t.Error("different passphrases produced the same key")
	}
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key := testKey(t)
	for _, plaintext := range [][]byte{
		[]byte("hello vault"),
		[]byte(""),
		bytes.Repeat([]byte{0xAB}, 64*1024), // a private-key-sized blob and then some
	} {
		ct, iv, tag, err := Encrypt(key, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if len(iv) != NonceSize {
			t.Fatalf("iv length = %d, want %d", len(iv), NonceSize)
		}
		if len(tag) != TagSize {
			t.Fatalf("tag length = %d, want %d", len(tag), TagSize)
		}
		if len(plaintext) > 0 && bytes.Contains(ct, plaintext) {
			t.Error("ciphertext contains plaintext")
		}

		got, err := Decrypt(key, ct, iv, tag)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Error("roundtrip mismatch")
		}
	}
}

func TestEncryptUsesFreshNonce(t *testing.T) {
	key := testKey(t)
	plaintext := []byte("same plaintext")

	ct1, iv1, _, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	ct2, iv2, _, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(iv1, iv2) {
		t.Error("two encryptions reused the same nonce")
	}
	if bytes.Equal(ct1, ct2) {
		t.Error("two encryptions of the same plaintext produced identical ciphertext")
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	key := testKey(t)
	ct, iv, tag, err := Encrypt(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	flip := func(b []byte) []byte {
		c := bytes.Clone(b)
		c[0] ^= 0xFF
		return c
	}

	cases := map[string]struct{ ct, iv, tag []byte }{
		"tampered ciphertext": {flip(ct), iv, tag},
		"tampered iv":         {ct, flip(iv), tag},
		"tampered tag":        {ct, iv, flip(tag)},
	}
	for name, c := range cases {
		if _, err := Decrypt(key, c.ct, c.iv, c.tag); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: error = %v, want ErrDecrypt", name, err)
		}
	}

	wrongKey := testKey(t)
	wrongKey[0] ^= 0xFF
	if _, err := Decrypt(wrongKey, ct, iv, tag); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong key: error = %v, want ErrDecrypt", err)
	}
}

func TestEncryptRejectsBadKeySize(t *testing.T) {
	if _, _, _, err := Encrypt([]byte("short"), []byte("x")); err == nil {
		t.Error("Encrypt accepted a key of the wrong size")
	}
	if _, err := Decrypt([]byte("short"), nil, nil, nil); err == nil {
		t.Error("Decrypt accepted a key of the wrong size")
	}
}

// testKey returns a deterministic-length random key without paying the
// Argon2id cost in every test.
func testKey(t *testing.T) []byte {
	t.Helper()
	salt, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	return salt[:KeySize]
}
