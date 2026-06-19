package tools

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"orbit/internal/audit"
	"orbit/internal/container"
	"orbit/internal/exec"
	"orbit/internal/keys"
	"orbit/internal/vault"
	"orbit/internal/vm"
)

// recordingDocker captures every docker invocation so tests can assert on the
// exact argv and environment the tool layer produced.
type recordingDocker struct {
	mu   sync.Mutex
	invs []container.Invocation
	out  func(inv container.Invocation) ([]byte, []byte, error)
}

func (r *recordingDocker) run(ctx context.Context, inv container.Invocation) ([]byte, []byte, error) {
	r.mu.Lock()
	r.invs = append(r.invs, inv)
	r.mu.Unlock()
	if r.out != nil {
		return r.out(inv)
	}
	return nil, nil, nil
}

func (r *recordingDocker) last() container.Invocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.invs[len(r.invs)-1]
}

// startSessionWithDocker wires a full server using a caller-supplied docker
// runner and returns both the client session and the live vault store (so
// tests can seed credentials for secret_env).
func startSessionWithDocker(t *testing.T, docker container.DockerRunner) (*mcp.ClientSession, *vault.Store) {
	t.Helper()
	store, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	km := keys.NewManager(store)
	deps := &Deps{
		Vault:      store,
		Keys:       km,
		VMs:        vm.NewManager(store, vm.WithRunner(fakeOrbctl)),
		Runner:     exec.NewRunner(store, km),
		Containers: container.NewManager(store, container.WithRunner(docker)),
		Audit:      audit.NewNop(),
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "orbit-test", Version: "0.0.0"}, nil)
	Register(server, deps)

	st, ct := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), st)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, store
}

// The crux of the container security model, exercised end-to-end through the
// MCP tool layer: a secret_env value must reach docker's environment but must
// never appear in argv (visible to `ps`) or in the tool result.
func TestContainerRunSecretEnvNeverHitsArgvViaTool(t *testing.T) {
	rec := &recordingDocker{out: func(inv container.Invocation) ([]byte, []byte, error) {
		if len(inv.Args) > 0 && inv.Args[0] == "run" {
			return []byte("deadbeef\n"), nil, nil
		}
		return nil, nil, nil
	}}
	cs, store := startSessionWithDocker(t, rec.run)

	if _, err := store.SetCredential("default", "prod-db-pw", []byte("hunter2-SECRET")); err != nil {
		t.Fatal(err)
	}

	out := call(t, cs, "container_run", map[string]any{
		"image":      "postgres:16",
		"secret_env": map[string]any{"POSTGRES_PASSWORD": "prod-db-pw"},
	})
	if out["container_id"] != "deadbeef" {
		t.Errorf("container_id = %v", out["container_id"])
	}

	inv := rec.last()
	argv := strings.Join(inv.Args, " ")
	if strings.Contains(argv, "hunter2-SECRET") {
		t.Fatalf("secret value leaked into argv: %s", argv)
	}
	if !strings.Contains(argv, "-e POSTGRES_PASSWORD") {
		t.Errorf("expected bare -e POSTGRES_PASSWORD flag, got: %s", argv)
	}
	if inv.Env["POSTGRES_PASSWORD"] != "hunter2-SECRET" {
		t.Errorf("secret value did not reach the docker subprocess env: %v", inv.Env)
	}
}

func TestContainerLifecycleToolsHappyPath(t *testing.T) {
	rec := &recordingDocker{out: func(inv container.Invocation) ([]byte, []byte, error) {
		if len(inv.Args) > 0 && inv.Args[0] == "run" {
			return []byte("abc123\n"), nil, nil
		}
		return []byte("ok"), nil, nil
	}}
	cs, _ := startSessionWithDocker(t, rec.run)

	run := call(t, cs, "container_run", map[string]any{"image": "alpine", "name": "box"})
	if run["container_id"] != "abc123" {
		t.Fatalf("container_run = %v", run)
	}
	call(t, cs, "container_exec", map[string]any{"name": "box", "command": "echo hi"})
	call(t, cs, "container_logs", map[string]any{"name": "box"})
	call(t, cs, "container_list", map[string]any{"all": true})

	stop := call(t, cs, "container_stop", map[string]any{"name": "box"})
	if stop["status"] != "stopped" {
		t.Errorf("container_stop = %v", stop)
	}
	rm := call(t, cs, "container_rm", map[string]any{"name": "box", "force": true})
	if rm["status"] != "removed" {
		t.Errorf("container_rm = %v", rm)
	}
}

func TestComposeUpInlineYAMLAndImageBuild(t *testing.T) {
	rec := &recordingDocker{}
	cs, _ := startSessionWithDocker(t, rec.run)

	up := call(t, cs, "compose_up", map[string]any{
		"project": "stack",
		"yaml":    "services:\n  web:\n    image: nginx\n",
	})
	if up["status"] != "up" {
		t.Errorf("compose_up = %v", up)
	}
	// Inline YAML must travel on stdin, not argv (see ADR-008).
	inv := rec.last()
	if len(inv.Stdin) == 0 {
		t.Error("inline compose YAML was not passed on stdin")
	}

	// compose_down requires a project definition too (file or yaml), matching
	// the uniform validation across the compose tools.
	down := call(t, cs, "compose_down", map[string]any{
		"project": "stack",
		"yaml":    "services:\n  web:\n    image: nginx\n",
	})
	if down["status"] != "down" {
		t.Errorf("compose_down = %v", down)
	}

	build := call(t, cs, "image_build", map[string]any{
		"tag":          "myapp:dev",
		"context_path": "/tmp/ctx",
		"dockerfile":   "FROM alpine\n",
	})
	if build["status"] != "built" {
		t.Errorf("image_build = %v", build)
	}
}
