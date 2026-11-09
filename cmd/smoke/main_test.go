package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/machugram/orbit/internal/exec"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "orbit-smoke-test-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	smokeBin = filepath.Join(dir, "smoke")
	cmd := osexec.Command("go", "build", "-o", smokeBin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		panic(string(out) + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var smokeBin string

func TestVMNameIsHex(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		name := vmName()
		if !re.MatchString(name) {
			t.Fatalf("vmName() = %q, want 8 hex chars", name)
		}
		seen[name] = true
	}
	if len(seen) < 2 {
		t.Fatalf("vmName() returned one value %v across 8 calls", seen)
	}
}

func TestRetryExecSucceedsAfterFailures(t *testing.T) {
	fake := &scriptedExec{fails: 2, result: &exec.Result{Stdout: "agent\n"}}
	res, err := retryExecFor(context.Background(), fake, "smoke-1", "id", time.Second, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "agent\n" || fake.calls != 3 {
		t.Fatalf("result=%+v calls=%d, want stdout on the third call", res, fake.calls)
	}
}

func TestRetryExecStopsAtDeadline(t *testing.T) {
	fake := &scriptedExec{fails: 100, err: fmt.Errorf("connection refused")}
	_, err := retryExecFor(context.Background(), fake, "smoke-1", "id", 20*time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v", err)
	}
	if fake.calls < 2 {
		t.Fatalf("calls = %d, want retries before the deadline", fake.calls)
	}
}

func TestRetryExecStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &scriptedExec{fails: 100, err: fmt.Errorf("connection refused")}
	_, err := retryExecFor(ctx, fake, "smoke-1", "id", time.Minute, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want a single attempt once the context is done", fake.calls)
	}
}

func TestExitsWhenOrbctlMissing(t *testing.T) {
	cmd := osexec.Command(smokeBin)
	cmd.Env = []string{"PATH=" + t.TempDir(), "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("smoke exited 0 without orbctl: %s", out)
	}
	if !strings.Contains(string(out), "orbctl not found in PATH") {
		t.Fatalf("stderr = %q", out)
	}
}

type scriptedExec struct {
	fails  int
	calls  int
	err    error
	result *exec.Result
}

func (s *scriptedExec) Exec(ctx context.Context, vmID, command string) (*exec.Result, error) {
	s.calls++
	if s.fails > 0 {
		s.fails--
		if s.err != nil {
			return nil, s.err
		}
		return nil, fmt.Errorf("not ready")
	}
	return s.result, nil
}
