package vault

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// newID returns a random 128-bit hex identifier. This matches the crypto/rand
// approach already used elsewhere (vm.randHex) and avoids an external UUID
// dependency — 128 random bits give the same collision resistance as UUIDv4.
// An error from the random source is surfaced rather than silently yielding an
// all-zero (and thus colliding) id.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("vault: generating id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

var (
	// ErrWrongPassphrase is returned by Open when the passphrase does not
	// match the one the vault was initialized with.
	ErrWrongPassphrase = errors.New("vault: wrong passphrase")
	// ErrNotFound is returned when a credential or VM record does not exist.
	ErrNotFound = errors.New("vault: not found")
)

// canaryPlaintext is encrypted with the master key at vault init time so
// Open can verify the passphrase before serving any reads.
const canaryPlaintext = "orbit-vault-v1"

const schema = `
CREATE TABLE IF NOT EXISTS vault_meta (
  k          TEXT PRIMARY KEY,
  v          BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS credentials (
  id          TEXT PRIMARY KEY,
  namespace   TEXT NOT NULL DEFAULT 'default',
  key         TEXT NOT NULL,
  ciphertext  BLOB NOT NULL,
  iv          BLOB NOT NULL,
  tag         BLOB NOT NULL,
  label       TEXT,
  vm_id       TEXT,
  created_at  INTEGER NOT NULL,
  rotated_at  INTEGER,
  UNIQUE(namespace, key)
);

CREATE TABLE IF NOT EXISTS vms (
  id           TEXT PRIMARY KEY,
  distro       TEXT NOT NULL,
  state        TEXT NOT NULL,
  key_id       TEXT,
  created_at   INTEGER NOT NULL,
  destroyed_at INTEGER
);

CREATE TABLE IF NOT EXISTS microvms (
  id           TEXT PRIMARY KEY,
  carrier      TEXT NOT NULL,
  image        TEXT NOT NULL,
  key_id       TEXT,
  ssh_port     INTEGER NOT NULL,
  handle       TEXT,
  state        TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  destroyed_at INTEGER
);
`

// Store is an encrypted credential vault backed by SQLite. All secret
// values are AES-256-GCM encrypted with a master key derived from the
// passphrase via Argon2id; only ciphertext touches disk.
type Store struct {
	db  *sql.DB
	key []byte
}

// Open opens (or initializes) the vault at path. On first open it generates
// an Argon2id salt and stores an encrypted canary; on subsequent opens it
// derives the key from the passphrase and verifies it against the canary,
// returning ErrWrongPassphrase on mismatch.
func Open(path string, passphrase []byte) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("vault: creating directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("vault: opening database: %w", err)
	}
	// modernc.org/sqlite serializes writes; a single connection avoids
	// SQLITE_BUSY between pooled connections.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("vault: setting pragmas: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("vault: applying schema: %w", err)
	}

	key, err := unlock(db, passphrase)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, key: key}, nil
}

// unlock derives the master key, initializing salt and canary on first run.
func unlock(db *sql.DB, passphrase []byte) ([]byte, error) {
	var salt []byte
	err := db.QueryRow(`SELECT v FROM vault_meta WHERE k = 'salt'`).Scan(&salt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return initVault(db, passphrase)
	case err != nil:
		return nil, fmt.Errorf("vault: reading salt: %w", err)
	}

	key := DeriveKey(passphrase, salt)

	var ct, iv, tag []byte
	if err := db.QueryRow(`SELECT v FROM vault_meta WHERE k = 'canary_ct'`).Scan(&ct); err != nil {
		return nil, fmt.Errorf("vault: reading canary: %w", err)
	}
	if err := db.QueryRow(`SELECT v FROM vault_meta WHERE k = 'canary_iv'`).Scan(&iv); err != nil {
		return nil, fmt.Errorf("vault: reading canary: %w", err)
	}
	if err := db.QueryRow(`SELECT v FROM vault_meta WHERE k = 'canary_tag'`).Scan(&tag); err != nil {
		return nil, fmt.Errorf("vault: reading canary: %w", err)
	}

	plain, err := Decrypt(key, ct, iv, tag)
	if err != nil || subtle.ConstantTimeCompare(plain, []byte(canaryPlaintext)) != 1 {
		return nil, ErrWrongPassphrase
	}
	return key, nil
}

func initVault(db *sql.DB, passphrase []byte) ([]byte, error) {
	salt, err := NewSalt()
	if err != nil {
		return nil, err
	}
	key := DeriveKey(passphrase, salt)

	ct, iv, tag, err := Encrypt(key, []byte(canaryPlaintext))
	if err != nil {
		return nil, err
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("vault: initializing: %w", err)
	}
	defer tx.Rollback()
	for k, v := range map[string][]byte{
		"salt": salt, "canary_ct": ct, "canary_iv": iv, "canary_tag": tag,
	} {
		if _, err := tx.Exec(`INSERT INTO vault_meta (k, v) VALUES (?, ?)`, k, v); err != nil {
			return nil, fmt.Errorf("vault: initializing: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("vault: initializing: %w", err)
	}
	return key, nil
}

// Close closes the underlying database and zeroes the in-memory master key.
func (s *Store) Close() error {
	for i := range s.key {
		s.key[i] = 0
	}
	return s.db.Close()
}

// SetCredential encrypts value and upserts it under (namespace, key). A
// fresh IV is used on every write. Overwriting an existing credential keeps
// its ID and created_at and sets rotated_at.
func (s *Store) SetCredential(namespace, key string, value []byte, opts ...SetOption) (string, error) {
	if namespace == "" {
		namespace = "default"
	}
	var o setOptions
	for _, opt := range opts {
		opt(&o)
	}

	ct, iv, tag, err := Encrypt(s.key, value)
	if err != nil {
		return "", err
	}

	now := time.Now().Unix()
	var id string
	err = s.db.QueryRow(
		`SELECT id FROM credentials WHERE namespace = ? AND key = ?`, namespace, key,
	).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, err = newID()
		if err != nil {
			return "", err
		}
		_, err = s.db.Exec(
			`INSERT INTO credentials (id, namespace, key, ciphertext, iv, tag, label, vm_id, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, namespace, key, ct, iv, tag, nullable(o.label), nullable(o.vmID), now,
		)
	case err == nil:
		_, err = s.db.Exec(
			`UPDATE credentials
			 SET ciphertext = ?, iv = ?, tag = ?, label = ?, vm_id = ?, rotated_at = ?
			 WHERE id = ?`,
			ct, iv, tag, nullable(o.label), nullable(o.vmID), now, id,
		)
	}
	if err != nil {
		return "", fmt.Errorf("vault: writing credential: %w", err)
	}
	return id, nil
}

// GetCredential decrypts and returns the value stored under (namespace, key).
func (s *Store) GetCredential(namespace, key string) ([]byte, error) {
	if namespace == "" {
		namespace = "default"
	}
	var ct, iv, tag []byte
	err := s.db.QueryRow(
		`SELECT ciphertext, iv, tag FROM credentials WHERE namespace = ? AND key = ?`,
		namespace, key,
	).Scan(&ct, &iv, &tag)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("vault: reading credential: %w", err)
	}
	return Decrypt(s.key, ct, iv, tag)
}

// GetCredentialByID decrypts and returns the credential with the given ID
// along with its metadata. Used when following key_id references from VM
// records.
func (s *Store) GetCredentialByID(id string) ([]byte, *CredentialInfo, error) {
	var c CredentialInfo
	var ct, iv, tag []byte
	err := s.db.QueryRow(
		`SELECT id, namespace, key, ciphertext, iv, tag, COALESCE(label, ''), COALESCE(vm_id, ''),
		        created_at, COALESCE(rotated_at, 0)
		 FROM credentials WHERE id = ?`, id,
	).Scan(&c.ID, &c.Namespace, &c.Key, &ct, &iv, &tag, &c.Label, &c.VMID, &c.CreatedAt, &c.RotatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("vault: reading credential: %w", err)
	}
	plain, err := Decrypt(s.key, ct, iv, tag)
	if err != nil {
		return nil, nil, err
	}
	return plain, &c, nil
}

// DeleteCredentialByID removes the credential with the given ID.
func (s *Store) DeleteCredentialByID(id string) error {
	res, err := s.db.Exec(`DELETE FROM credentials WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("vault: deleting credential: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteCredential removes the credential under (namespace, key).
func (s *Store) DeleteCredential(namespace, key string) error {
	if namespace == "" {
		namespace = "default"
	}
	res, err := s.db.Exec(
		`DELETE FROM credentials WHERE namespace = ? AND key = ?`, namespace, key,
	)
	if err != nil {
		return fmt.Errorf("vault: deleting credential: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListCredentials returns metadata (never values) for all credentials in a
// namespace, ordered by key.
func (s *Store) ListCredentials(namespace string) ([]CredentialInfo, error) {
	if namespace == "" {
		namespace = "default"
	}
	rows, err := s.db.Query(
		`SELECT id, namespace, key, COALESCE(label, ''), COALESCE(vm_id, ''),
		        created_at, COALESCE(rotated_at, 0)
		 FROM credentials WHERE namespace = ? ORDER BY key`, namespace,
	)
	if err != nil {
		return nil, fmt.Errorf("vault: listing credentials: %w", err)
	}
	defer rows.Close()

	var infos []CredentialInfo
	for rows.Next() {
		var c CredentialInfo
		if err := rows.Scan(&c.ID, &c.Namespace, &c.Key, &c.Label, &c.VMID, &c.CreatedAt, &c.RotatedAt); err != nil {
			return nil, fmt.Errorf("vault: listing credentials: %w", err)
		}
		infos = append(infos, c)
	}
	return infos, rows.Err()
}

// CreateVM inserts a VM record in the "creating" state.
func (s *Store) CreateVM(id, distro string) error {
	_, err := s.db.Exec(
		`INSERT INTO vms (id, distro, state, created_at) VALUES (?, ?, ?, ?)`,
		id, distro, string(VMCreating), time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("vault: creating vm record: %w", err)
	}
	return nil
}

// GetVM returns the VM record with the given id.
func (s *Store) GetVM(id string) (*VM, error) {
	var vm VM
	var state string
	err := s.db.QueryRow(
		`SELECT id, distro, state, COALESCE(key_id, ''), created_at, COALESCE(destroyed_at, 0)
		 FROM vms WHERE id = ?`, id,
	).Scan(&vm.ID, &vm.Distro, &state, &vm.KeyID, &vm.CreatedAt, &vm.DestroyedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("vault: reading vm record: %w", err)
	}
	vm.State = VMState(state)
	return &vm, nil
}

// ListVMs returns all VM records, newest first.
func (s *Store) ListVMs() ([]VM, error) {
	rows, err := s.db.Query(
		`SELECT id, distro, state, COALESCE(key_id, ''), created_at, COALESCE(destroyed_at, 0)
		 FROM vms ORDER BY created_at DESC, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("vault: listing vms: %w", err)
	}
	defer rows.Close()

	var vms []VM
	for rows.Next() {
		var vm VM
		var state string
		if err := rows.Scan(&vm.ID, &vm.Distro, &state, &vm.KeyID, &vm.CreatedAt, &vm.DestroyedAt); err != nil {
			return nil, fmt.Errorf("vault: listing vms: %w", err)
		}
		vm.State = VMState(state)
		vms = append(vms, vm)
	}
	return vms, rows.Err()
}

// SetVMState updates a VM's lifecycle state. Transitioning to VMDestroyed
// also records destroyed_at.
func (s *Store) SetVMState(id string, state VMState) error {
	var res sql.Result
	var err error
	if state == VMDestroyed {
		res, err = s.db.Exec(
			`UPDATE vms SET state = ?, destroyed_at = ? WHERE id = ?`,
			string(state), time.Now().Unix(), id,
		)
	} else {
		res, err = s.db.Exec(`UPDATE vms SET state = ? WHERE id = ?`, string(state), id)
	}
	if err != nil {
		return fmt.Errorf("vault: updating vm state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetVMKey associates a credential (SSH keypair) with a VM.
func (s *Store) SetVMKey(id, keyID string) error {
	res, err := s.db.Exec(`UPDATE vms SET key_id = ? WHERE id = ?`, keyID, id)
	if err != nil {
		return fmt.Errorf("vault: updating vm key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateMicroVM inserts a nested-guest record in the "creating" state.
func (s *Store) CreateMicroVM(mv MicroVM) error {
	_, err := s.db.Exec(
		`INSERT INTO microvms (id, carrier, image, key_id, ssh_port, handle, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		mv.ID, mv.Carrier, mv.Image, nullable(mv.KeyID), mv.SSHPort,
		nullable(mv.Handle), string(VMCreating), time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("vault: creating microvm record: %w", err)
	}
	return nil
}

// GetMicroVM returns the microVM record with the given id.
func (s *Store) GetMicroVM(id string) (*MicroVM, error) {
	var mv MicroVM
	var state string
	err := s.db.QueryRow(
		`SELECT id, carrier, image, COALESCE(key_id, ''), ssh_port, COALESCE(handle, ''),
		        state, created_at, COALESCE(destroyed_at, 0)
		 FROM microvms WHERE id = ?`, id,
	).Scan(&mv.ID, &mv.Carrier, &mv.Image, &mv.KeyID, &mv.SSHPort, &mv.Handle,
		&state, &mv.CreatedAt, &mv.DestroyedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("vault: reading microvm record: %w", err)
	}
	mv.State = VMState(state)
	return &mv, nil
}

// ListMicroVMs returns all microVM records, newest first.
func (s *Store) ListMicroVMs() ([]MicroVM, error) {
	rows, err := s.db.Query(
		`SELECT id, carrier, image, COALESCE(key_id, ''), ssh_port, COALESCE(handle, ''),
		        state, created_at, COALESCE(destroyed_at, 0)
		 FROM microvms ORDER BY created_at DESC, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("vault: listing microvms: %w", err)
	}
	defer rows.Close()

	var out []MicroVM
	for rows.Next() {
		var mv MicroVM
		var state string
		if err := rows.Scan(&mv.ID, &mv.Carrier, &mv.Image, &mv.KeyID, &mv.SSHPort,
			&mv.Handle, &state, &mv.CreatedAt, &mv.DestroyedAt); err != nil {
			return nil, fmt.Errorf("vault: listing microvms: %w", err)
		}
		mv.State = VMState(state)
		out = append(out, mv)
	}
	return out, rows.Err()
}

// SetMicroVMState updates a microVM's lifecycle state, recording destroyed_at
// on the transition to VMDestroyed.
func (s *Store) SetMicroVMState(id string, state VMState) error {
	var res sql.Result
	var err error
	if state == VMDestroyed {
		res, err = s.db.Exec(
			`UPDATE microvms SET state = ?, destroyed_at = ? WHERE id = ?`,
			string(state), time.Now().Unix(), id,
		)
	} else {
		res, err = s.db.Exec(`UPDATE microvms SET state = ? WHERE id = ?`, string(state), id)
	}
	if err != nil {
		return fmt.Errorf("vault: updating microvm state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
