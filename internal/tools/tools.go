// Package tools is the MCP surface an agent actually calls.
//
// The tools are jobs: run something, open or close a workspace, bring a
// service up or down, move a file, store a secret, or converge declared
// state. Inventory and diagnostics are resources (orbit://workspaces,
// orbit://secrets, orbit://info, orbit://doctor), not tools the model has
// to chain like a CLI.
package tools

import (
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/audit"
	"github.com/machugram/orbit/internal/container"
	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/microvm"
	"github.com/machugram/orbit/internal/vault"
	"github.com/machugram/orbit/internal/vm"
)

// Deps carries the subsystems the tool handlers operate on.
type Deps struct {
	Version    string
	VaultPath  string
	AuditPath  string
	Vault      *vault.Store
	Keys       *keys.Manager
	VMs        *vm.Manager
	Prov       *vm.Provisioner
	Runner     *exec.Runner
	Containers *container.Manager
	MicroVMs   *microvm.Manager
	Audit      *audit.Logger
}

// Register adds Orbit's tools and resources to the server.
func Register(server *mcp.Server, d *Deps) {
	registerRun(server, d)
	registerWorkspace(server, d)
	registerService(server, d)
	registerFile(server, d)
	registerSecret(server, d)
	registerConverge(server, d)
	registerResources(server, d)
}

// log writes an audit entry for one tool call. detail must never contain
// secret values.
func (d *Deps) log(tool string, detail map[string]any, err error) {
	if d.Audit != nil {
		d.Audit.Log(tool, detail, err)
	}
}

// jsonResult renders v as pretty JSON text content.
func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		b = []byte(`{"error":"failed to encode result"}`)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}
}

func resourceJSON(uri string, v any) (*mcp.ReadResourceResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(b),
		}},
	}, nil
}
