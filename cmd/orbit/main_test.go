package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "orbit-cmd-test-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	orbitBin = filepath.Join(dir, "orbit")
	cmd := exec.Command("go", "build", "-o", orbitBin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		panic(string(out) + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var orbitBin string

func TestEnvOr(t *testing.T) {
	t.Setenv("ORBIT_TEST_ENV", "")
	if got := envOr("ORBIT_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("empty env = %q, want fallback", got)
	}
	t.Setenv("ORBIT_TEST_ENV", "set")
	if got := envOr("ORBIT_TEST_ENV", "fallback"); got != "set" {
		t.Fatalf("set env = %q, want set", got)
	}
}

func TestVersionFlag(t *testing.T) {
	for _, arg := range []string{"--version", "-version"} {
		t.Run(arg, func(t *testing.T) {
			out, err := exec.Command(orbitBin, arg).CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", arg, err, out)
			}
			if strings.TrimSpace(string(out)) != "orbit "+version {
				t.Fatalf("%s printed %q", arg, out)
			}
		})
	}
}

func TestBinaryServesJobs(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(orbitBin)
	cmd.Env = withoutOrbitEnv(
		"ORBIT_PASSPHRASE=test-pass",
		"ORBIT_VAULT_PATH="+filepath.Join(dir, "vault.db"),
		"ORBIT_AUDIT_PATH="+filepath.Join(dir, "audit.log"),
		"ORBIT_AGENT_NAMESPACE=cmdtest",
	)
	cmd.Stderr = os.Stderr

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "cmd-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })

	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range listed.Tools {
		got[tool.Name] = true
	}
	for _, name := range []string{
		"run", "workspace_open", "workspace_close",
		"service_up", "service_down", "file_put", "file_get",
		"secret_put", "converge",
	} {
		if !got[name] {
			t.Errorf("binary missing tool %s", name)
		}
	}
	if len(listed.Tools) != 9 {
		t.Errorf("binary registered %d tools, want 9", len(listed.Tools))
	}

	info, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "orbit://info"})
	if err != nil {
		t.Fatal(err)
	}
	body := info.Contents[0].Text
	if !strings.Contains(body, `"version": "`+version+`"`) || !strings.Contains(body, dir) {
		t.Fatalf("orbit://info = %s", body)
	}

	put, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "secret_put",
		Arguments: map[string]any{"key": "api-token", "value": "s3cret"},
	})
	if err != nil || put.IsError {
		t.Fatalf("secret_put: err=%v result=%v", err, put)
	}
	text := put.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, "s3cret") {
		t.Fatalf("secret_put returned the value: %s", text)
	}
	secrets, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "orbit://secrets"})
	if err != nil {
		t.Fatal(err)
	}
	names := secrets.Contents[0].Text
	if !strings.Contains(names, "api-token") || strings.Contains(names, "s3cret") {
		t.Fatalf("orbit://secrets = %s", names)
	}
}

func withoutOrbitEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "ORBIT_") {
			continue
		}
		env = append(env, e)
	}
	return append(env, extra...)
}
