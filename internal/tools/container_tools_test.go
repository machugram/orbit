package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/audit"
	"github.com/machugram/orbit/internal/container"
	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/vault"
	"github.com/machugram/orbit/internal/vm"
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

func (r *recordingDocker) calls() []container.Invocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]container.Invocation, len(r.invs))
	copy(out, r.invs)
	return out
}

func findCall(calls []container.Invocation, first string) (container.Invocation, bool) {
	for _, inv := range calls {
		if len(inv.Args) > 0 && inv.Args[0] == first {
			return inv, true
		}
	}
	return container.Invocation{}, false
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

	out := call(t, cs, "service_up", map[string]any{
		"image":      "postgres:16",
		"name":       "db",
		"secret_env": map[string]any{"POSTGRES_PASSWORD": "prod-db-pw"},
	})
	if out["name"] != "db" || strings.Contains(fmt.Sprint(out), "hunter2-SECRET") {
		t.Fatalf("service_up = %v", out)
	}

	inv, ok := findCall(rec.calls(), "run")
	if !ok {
		t.Fatal("docker run was not invoked")
	}
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
		if len(inv.Args) > 0 && inv.Args[0] == "ps" {
			return []byte("{\"ID\":\"abc\",\"Names\":\"box\",\"Image\":\"alpine\",\"State\":\"running\",\"Status\":\"Up\",\"Ports\":\"\"}\n"), nil, nil
		}
		return []byte("ok"), nil, nil
	}}
	cs, _ := startSessionWithDocker(t, rec.run)

	up := call(t, cs, "service_up", map[string]any{"image": "alpine", "name": "box"})
	if up["name"] != "box" || up["host"] != "box.orb.local" {
		t.Fatalf("service_up = %v", up)
	}
	ran := call(t, cs, "run", map[string]any{"target": "box", "command": "echo hi"})
	if ran["target"] != "box" || ran["exit_code"] != float64(0) {
		t.Fatalf("run = %v", ran)
	}
	down := call(t, cs, "service_down", map[string]any{"name": "box"})
	if down["name"] != "box" {
		t.Errorf("service_down = %v", down)
	}
}

func TestComposeUpInlineYAMLAndImageBuild(t *testing.T) {
	rec := &recordingDocker{}
	cs, _ := startSessionWithDocker(t, rec.run)

	up := call(t, cs, "service_up", map[string]any{
		"project": "stack",
		"yaml":    "services:\n  web:\n    image: nginx\n",
	})
	if up["project"] != "stack" || up["kind"] != "compose" {
		t.Errorf("service_up = %v", up)
	}
	// Inline YAML must travel on stdin, not argv (see ADR-008).
	var sawStdin bool
	for _, inv := range rec.calls() {
		if len(inv.Stdin) > 0 && strings.Contains(string(inv.Stdin), "image: nginx") {
			sawStdin = true
			if strings.Contains(strings.Join(inv.Args, " "), "image: nginx") {
				t.Errorf("inline YAML leaked into argv: %v", inv.Args)
			}
		}
	}
	if !sawStdin {
		t.Error("inline compose YAML was not passed on stdin")
	}

	down := call(t, cs, "service_down", map[string]any{"project": "stack"})
	if down["project"] != "stack" {
		t.Errorf("service_down = %v", down)
	}
	inv := rec.last()
	if strings.Join(inv.Args, " ") != "compose -p stack down" {
		t.Errorf("service_down args = %v", inv.Args)
	}
}
