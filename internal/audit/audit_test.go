package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLogWritesJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "audit.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	l.Log("vm_create", map[string]any{"distro": "ubuntu"}, nil)
	l.Log("exec", map[string]any{"vm_id": "x"}, errors.New("boom"))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var entries []entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		entries = append(entries, e)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Tool != "vm_create" || !entries[0].OK || entries[0].TS == "" {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[1].Tool != "exec" || entries[1].OK || entries[1].Error != "boom" {
		t.Errorf("entry 1 = %+v", entries[1])
	}

	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("audit log mode = %v, want 0600", st.Mode().Perm())
	}
}
