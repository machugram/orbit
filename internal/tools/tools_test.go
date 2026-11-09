package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/audit"
	"github.com/machugram/orbit/internal/container"
	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/microvm"
	"github.com/machugram/orbit/internal/vault"
	"github.com/machugram/orbit/internal/vm"
)

// fakeOrbctl simulates a healthy orbctl for tool-level tests.
func fakeOrbctl(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "create", "clone", "delete", "start", "stop":
		return nil, nil, nil
	case "info":
		return []byte(fmt.Sprintf(`{"name":%q,"state":"running"}`, args[1])), nil, nil
	case "list":
		return []byte(`[]`), nil, nil
	}
	return nil, nil, fmt.Errorf("fake orbctl: %v", args)
}

// startSession wires a full server (with fakes) to an in-memory MCP client.
func startSession(t *testing.T) *mcp.ClientSession {
	t.Helper()

	store, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	km := keys.NewManager(store)
	vmm := vm.NewManager(store, vm.WithRunner(fakeOrbctl), vm.WithPollInterval(time.Millisecond))
	fakeExec := func(ctx context.Context, vmName, user, script string, args ...string) ([]byte, []byte, error) {
		return []byte("ORBIT_PROVISIONED\n"), nil, nil
	}
	fakeDocker := func(ctx context.Context, inv container.Invocation) ([]byte, []byte, error) {
		if len(inv.Args) > 0 && inv.Args[0] == "run" {
			return []byte("c0ffee\n"), nil, nil
		}
		return nil, nil, nil
	}
	resolver := microvm.NewResolver(store, km, exec.VaultResolver(store, km))
	deps := &Deps{
		Version:    "test",
		VaultPath:  filepath.Join(t.TempDir(), "vault.db"),
		AuditPath:  filepath.Join(t.TempDir(), "audit.log"),
		Vault:      store,
		Keys:       km,
		VMs:        vmm,
		Prov:       vm.NewProvisioner(km, vmm, vm.WithVMExecer(fakeExec)),
		Runner:     exec.NewRunnerWithResolver(resolver),
		Containers: container.NewManager(store, container.WithRunner(fakeDocker)),
		MicroVMs: microvm.NewManager(store, km,
			microvm.WithCarrierRunner(func(context.Context, string, string, ...string) ([]byte, []byte, error) {
				return nil, nil, nil
			}),
			microvm.WithReadyCheck(func(context.Context, string) error { return nil }),
		),
		Audit: audit.NewNop(),
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
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s returned tool error: %v", tool, res.Content)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: result is not a JSON object: %s", tool, text)
	}
	return out
}

func callRaw(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s returned tool error: %v", tool, res.Content)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func mustFail(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return
	}
	if res == nil || !res.IsError {
		t.Fatalf("%s should have failed: %+v", tool, res)
	}
}

func readResource(t *testing.T, cs *mcp.ClientSession, uri string) string {
	t.Helper()
	res, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("read %s: %v", uri, err)
	}
	if len(res.Contents) == 0 || res.Contents[0].Text == "" {
		t.Fatalf("read %s: empty", uri)
	}
	return res.Contents[0].Text
}

func TestToolsAreJobs(t *testing.T) {
	cs := startSession(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	want := []string{
		"run",
		"workspace_open", "workspace_close",
		"service_up", "service_down",
		"file_put", "file_get",
		"secret_put",
		"converge",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q not registered", name)
		}
	}
	if len(res.Tools) != len(want) {
		names := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		t.Errorf("registered %d tools, want %d: %s", len(res.Tools), len(want), strings.Join(names, ", "))
	}
	for _, gone := range []string{"vm_create", "vm_list", "cred_get", "key_generate", "exec", "state_plan", "orbit_info", "container_run"} {
		if got[gone] {
			t.Errorf("control-plane tool %q is still registered", gone)
		}
	}
}

func TestRunSchemaAndThrowawayContainer(t *testing.T) {
	cs := startSession(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "run" && tool.Name != "service_up" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		schema := string(raw)
		if !strings.Contains(schema, `"secret_env"`) {
			t.Fatalf("%s schema missing secret_env: %s", tool.Name, schema)
		}
		for _, gone := range []string{`"namespace"`, `"max_concurrent"`, `"ttl_seconds"`, `"scale"`} {
			if strings.Contains(schema, gone) {
				t.Fatalf("%s schema still has %s: %s", tool.Name, gone, schema)
			}
		}
	}

	got := call(t, cs, "run", map[string]any{"command": "echo hi"})
	if got["kind"] != "container" || got["removed"] != true {
		t.Fatalf("throwaway run = %v", got)
	}
	if !strings.Contains(fmt.Sprint(got["stdout"]), "c0ffee") {
		t.Fatalf("stdout = %v", got["stdout"])
	}
	if got["exit_code"] != float64(0) {
		t.Fatalf("exit_code = %v", got["exit_code"])
	}

	mustFail(t, cs, "run", map[string]any{})
	mustFail(t, cs, "run", map[string]any{"command": "true", "script": "true"})
	mustFail(t, cs, "run", map[string]any{"command": "true", "isolation": "pod"})
	mustFail(t, cs, "run", map[string]any{"command": "true", "targets": []any{"missing"}})
}

func TestWorkspaceOpenCloseHidesKeys(t *testing.T) {
	cs := startSession(t)

	created := call(t, cs, "workspace_open", map[string]any{"kind": "vm", "distro": "ubuntu", "name": "tool-vm"})
	if created["name"] != "tool-vm" || created["kind"] != "vm" || created["state"] != "running" {
		t.Fatalf("workspace_open = %v", created)
	}
	pub, _ := created["public_key"].(string)
	if !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("public_key = %q", pub)
	}
	if _, ok := created["key_id"]; ok {
		t.Fatalf("workspace_open returned key_id: %v", created)
	}
	if strings.Contains(fmt.Sprint(created), "PRIVATE") {
		t.Fatal("workspace_open leaked private key material")
	}

	body := readResource(t, cs, "orbit://workspaces")
	if !strings.Contains(body, "tool-vm") {
		t.Fatalf("workspaces missing tool-vm: %s", body)
	}
	if strings.Contains(body, "orbit-base-") {
		t.Fatalf("workspaces listed a warm base: %s", body)
	}
	if strings.Contains(body, "key_id") || strings.Contains(body, pub) {
		t.Fatalf("workspaces leaked key material: %s", body)
	}

	closed := call(t, cs, "workspace_close", map[string]any{"name": "tool-vm"})
	if closed["state"] != "destroyed" {
		t.Fatalf("workspace_close = %v", closed)
	}
	body = readResource(t, cs, "orbit://workspaces")
	if strings.Contains(body, "tool-vm") {
		t.Fatalf("destroyed workspace still listed: %s", body)
	}

	reopened := call(t, cs, "workspace_open", map[string]any{"kind": "vm", "distro": "ubuntu", "name": "tool-vm"})
	if reopened["name"] != "tool-vm" || reopened["state"] != "running" {
		t.Fatalf("reopen = %v", reopened)
	}
	reopenedKey, _ := reopened["public_key"].(string)
	if !strings.HasPrefix(reopenedKey, "ssh-ed25519 ") || reopenedKey == pub {
		t.Fatalf("reopen public_key = %q, previous %q", reopenedKey, pub)
	}
}

func TestSecretPutIsWriteOnly(t *testing.T) {
	cs := startSession(t)
	got := call(t, cs, "secret_put", map[string]any{"key": "api-token", "value": "s3cret"})
	if got["status"] != "stored" || got["key"] != "api-token" {
		t.Fatalf("secret_put = %v", got)
	}
	if strings.Contains(fmt.Sprint(got), "s3cret") {
		t.Fatalf("secret_put returned the value: %v", got)
	}
	body := readResource(t, cs, "orbit://secrets")
	if !strings.Contains(body, "api-token") || strings.Contains(body, "s3cret") {
		t.Fatalf("secrets resource = %s", body)
	}
}

func TestServiceUpAndDown(t *testing.T) {
	cs := startSession(t)
	up := call(t, cs, "service_up", map[string]any{"image": "nginx:1.27", "name": "web", "ports": []any{"8080:80"}})
	if up["name"] != "web" || up["host"] != "web.orb.local" || up["kind"] != "container" {
		t.Fatalf("service_up = %v", up)
	}
	down := call(t, cs, "service_down", map[string]any{"name": "web"})
	if down["name"] != "web" {
		t.Fatalf("service_down = %v", down)
	}

	stack := call(t, cs, "service_up", map[string]any{
		"project": "demo",
		"yaml":    "services:\n  web:\n    image: nginx\n",
	})
	if stack["project"] != "demo" || stack["kind"] != "compose" {
		t.Fatalf("compose service_up = %v", stack)
	}
	call(t, cs, "service_down", map[string]any{"project": "demo"})
	mustFail(t, cs, "service_up", map[string]any{"image": "nginx", "project": "demo"})
}

func TestConvergeSchemaAndApply(t *testing.T) {
	cs := startSession(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	for _, tool := range res.Tools {
		if tool.Name != "converge" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		schema = string(raw)
	}
	if strings.Contains(schema, `"type":"array"`) {
		t.Fatalf("converge schema regressed to array: %s", schema)
	}
	for _, field := range []string{`"vms"`, `"containers"`, `"composes"`} {
		if !strings.Contains(schema, field) {
			t.Fatalf("converge schema missing %s: %s", field, schema)
		}
	}

	got := call(t, cs, "converge", map[string]any{
		"state": map[string]any{
			"vms": []any{map[string]any{"name": "lab", "distro": "ubuntu"}},
		},
	})
	if !strings.Contains(fmt.Sprint(got["summary"]), "applied") {
		t.Fatalf("converge = %v", got)
	}
	body := readResource(t, cs, "orbit://workspaces")
	if !strings.Contains(body, "lab") {
		t.Fatalf("workspaces after converge: %s", body)
	}
}

func TestInfoResource(t *testing.T) {
	cs := startSession(t)
	listed, err := cs.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	uris := map[string]bool{}
	for _, r := range listed.Resources {
		uris[r.URI] = true
	}
	for _, uri := range []string{"orbit://info", "orbit://doctor", "orbit://workspaces", "orbit://secrets"} {
		if !uris[uri] {
			t.Errorf("resource %s not listed: %+v", uri, listed.Resources)
		}
	}
	body := readResource(t, cs, "orbit://info")
	if !strings.Contains(body, `"version": "test"`) || !strings.Contains(body, "vault_path") {
		t.Fatalf("info = %s", body)
	}
}

func TestNamedRunCreatesContainerWorkspace(t *testing.T) {
	cs := startSession(t)
	got := call(t, cs, "run", map[string]any{"target": "box", "command": "echo hi"})
	if got["target"] != "box" || got["kind"] != "container" || got["created"] != true {
		t.Fatalf("named run = %v", got)
	}
	if got["removed"] == true {
		t.Fatalf("named run was removed: %v", got)
	}
}

func TestFilePutRequiresWorkspace(t *testing.T) {
	cs := startSession(t)
	mustFail(t, cs, "file_put", map[string]any{"target": "missing", "path": "/tmp/a", "content": "hi"})
	text := callRaw(t, cs, "run", map[string]any{"command": "printf secret-value"})
	if strings.Contains(text, "secret_env") {
		t.Fatalf("run result mentioned secret_env: %s", text)
	}
}
