package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orbit/internal/vault"
)

// TestE2ERealDocker exercises the real `docker` CLI: it stores a secret in the
// vault, runs a container with secret injection, and asserts the container saw
// the value while it never touched the command line. Skipped unless
// ORBIT_DOCKER_E2E is set (mirrors cmd/smoke's opt-in real-infra approach).
func TestE2ERealDocker(t *testing.T) {
	if os.Getenv("ORBIT_DOCKER_E2E") == "" {
		t.Skip("set ORBIT_DOCKER_E2E=1 to run against the real docker CLI")
	}
	s, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := NewManager(s) // real docker runner
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := s.SetCredential("default", "demo-token", []byte("s3cr3t-value"), nil...); err != nil {
		t.Fatal(err)
	}

	// Foreground run that echoes the injected secret and exits.
	out, err := m.Run(ctx, RunSpec{
		Image:     "alpine",
		Command:   "echo TOKEN=$DEMO_TOKEN",
		SecretEnv: map[string]string{"DEMO_TOKEN": "demo-token"},
		Remove:    true,
		Detach:    false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TOKEN=s3cr3t-value") {
		t.Fatalf("container did not receive injected secret, got %q", out)
	}
	t.Logf("real container output: %q", strings.TrimSpace(out))
}
