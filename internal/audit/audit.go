// Package audit appends one JSON line per MCP tool call to an audit log
// (spec §10: all tool calls logged to ~/.orbit/audit.log). Secret
// values are never logged — callers pass only metadata.
package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger writes audit entries. The zero-value-like NewNop logger discards.
type Logger struct {
	mu sync.Mutex
	w  io.WriteCloser
}

// Open creates (0600, parents made) or appends to the audit log at path.
func Open(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{w: f}, nil
}

// NewNop returns a Logger that discards all entries (used in tests).
func NewNop() *Logger {
	return &Logger{w: nopCloser{io.Discard}}
}

type entry struct {
	TS     string         `json:"ts"`
	Tool   string         `json:"tool"`
	Detail map[string]any `json:"detail,omitempty"`
	OK     bool           `json:"ok"`
	Error  string         `json:"error,omitempty"`
}

// Log records one tool invocation. detail must not contain secret values.
func (l *Logger) Log(tool string, detail map[string]any, err error) {
	e := entry{
		TS:     time.Now().UTC().Format(time.RFC3339Nano),
		Tool:   tool,
		Detail: detail,
		OK:     err == nil,
	}
	if err != nil {
		e.Error = err.Error()
	}
	line, jerr := json.Marshal(e)
	if jerr != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// The audit log is a compliance artifact; a dropped or short write must not
	// pass silently. We can't fail the tool call here, so surface it to stderr
	// (which never carries secret values — detail is metadata only).
	line = append(line, '\n')
	if n, err := l.w.Write(line); err != nil {
		fmt.Fprintf(os.Stderr, "orbit: audit log write failed for %q: %v\n", tool, err)
	} else if n < len(line) {
		fmt.Fprintf(os.Stderr, "orbit: audit log short write for %q: wrote %d of %d bytes\n", tool, n, len(line))
	}
}

// Close closes the underlying file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Close()
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
