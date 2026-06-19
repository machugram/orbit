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

	"orbit/internal/audit"
	"orbit/internal/container"
	"orbit/internal/exec"
	"orbit/internal/keys"
	"orbit/internal/vault"
	"orbit/internal/vm"
)

// fakeOrbctl simulates a healthy orbctl for tool-level tests.
func fakeOrbctl(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "create", "delete", "start", "stop":
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
	// Fake in-VM provisioning: report success without touching real orb.
	fakeExec := func(ctx context.Context, vmName, user, script string, args ...string) ([]byte, []byte, error) {
		return []byte("ORBIT_PROVISIONED\n"), nil, nil
	}
	// Fake docker: echo the container id on run, empty otherwise.
	fakeDocker := func(ctx context.Context, inv container.Invocation) ([]byte, []byte, error) {
		if len(inv.Args) > 0 && inv.Args[0] == "run" {
			return []byte("c0ffee\n"), nil, nil
		}
		return nil, nil, nil
	}
	deps := &Deps{
		Vault:      store,
		Keys:       km,
		VMs:        vmm,
		Prov:       vm.NewProvisioner(km, vmm, vm.WithVMExecer(fakeExec)),
		Runner:     exec.NewRunner(store, km),
		Containers: container.NewManager(store, container.WithRunner(fakeDocker)),
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

func TestAllSpecToolsRegistered(t *testing.T) {
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
		"vm_create", "vm_list", "vm_start", "vm_stop", "vm_destroy",
		"key_generate", "key_list", "key_rotate", "key_revoke", "key_export_public",
		"exec", "exec_script", "file_push", "file_pull", "exec_parallel",
		"cred_set", "cred_get", "cred_delete", "cred_list",
		"container_run", "container_exec", "container_logs", "container_list",
		"container_stop", "container_rm",
		"compose_up", "compose_down", "compose_logs", "image_build",
		"compose_ps", "compose_exec", "compose_build",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q not registered", name)
		}
	}
	if len(res.Tools) != len(want) {
		t.Errorf("registered %d tools, expected %d", len(res.Tools), len(want))
	}
}

func TestCredToolsRoundtrip(t *testing.T) {
	cs := startSession(t)

	call(t, cs, "cred_set", map[string]any{"key": "api-token", "value": "s3cret", "namespace": "ci"})
	got := call(t, cs, "cred_get", map[string]any{"key": "api-token", "namespace": "ci"})
	if got["value"] != "s3cret" {
		t.Errorf("cred_get = %v", got)
	}

	// cred_list must expose names, never values.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "cred_list", Arguments: map[string]any{"namespace": "ci"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "api-token") {
		t.Errorf("cred_list missing key name: %s", text)
	}
	if strings.Contains(text, "s3cret") {
		t.Errorf("cred_list leaked a secret value: %s", text)
	}

	call(t, cs, "cred_delete", map[string]any{"key": "api-token", "namespace": "ci"})
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "cred_get", Arguments: map[string]any{"key": "api-token", "namespace": "ci"},
	}); err == nil {
		res, _ := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "cred_get", Arguments: map[string]any{"key": "api-token", "namespace": "ci"},
		})
		if res == nil || !res.IsError {
			t.Error("cred_get after delete should fail")
		}
	}
}

func TestKeyToolsNeverExposePrivateKey(t *testing.T) {
	cs := startSession(t)

	gen := call(t, cs, "key_generate", map[string]any{"label": "test-key"})
	pub, _ := gen["public_key"].(string)
	if !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Errorf("public_key = %q", pub)
	}
	if s := fmt.Sprint(gen); strings.Contains(s, "PRIVATE KEY") {
		t.Error("key_generate leaked private key material")
	}

	exp := call(t, cs, "key_export_public", map[string]any{"label": "test-key"})
	if exp["public_key"] != pub {
		t.Errorf("key_export_public = %v, want %q", exp["public_key"], pub)
	}

	call(t, cs, "key_revoke", map[string]any{"label": "test-key"})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "key_export_public", Arguments: map[string]any{"label": "test-key"},
	})
	if err == nil && !res.IsError {
		t.Error("key_export_public after revoke should fail")
	}
}

func TestVMCreateToolProvisionsAndDestroys(t *testing.T) {
	cs := startSession(t)

	created := call(t, cs, "vm_create", map[string]any{"distro": "ubuntu", "name": "tool-vm"})
	if created["vm_id"] != "tool-vm" || created["state"] != "running" {
		t.Fatalf("vm_create = %v", created)
	}
	if created["key_id"] == "" || created["key_id"] == nil {
		t.Error("vm_create did not link a key")
	}
	if created["ssh_user"] != "agent" {
		t.Errorf("ssh_user = %v", created["ssh_user"])
	}

	// The generated keypair must be listable and exportable.
	exp := call(t, cs, "key_export_public", map[string]any{"label": "vm-tool-vm"})
	if !strings.HasPrefix(exp["public_key"].(string), "ssh-ed25519 ") {
		t.Errorf("exported key = %v", exp)
	}

	destroyed := call(t, cs, "vm_destroy", map[string]any{"vm_id": "tool-vm", "purge_keys": true})
	if destroyed["state"] != "destroyed" {
		t.Errorf("vm_destroy = %v", destroyed)
	}
	// Key must be purged with the VM.
	res, _ := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "key_export_public", Arguments: map[string]any{"label": "vm-tool-vm"},
	})
	if res == nil || !res.IsError {
		t.Error("key survived vm_destroy purge_keys=true")
	}
}
