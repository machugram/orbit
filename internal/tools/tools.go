// Package tools registers Orbit's MCP tools (spec §2) on an MCP server.
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
	Vault      *vault.Store
	Keys       *keys.Manager
	VMs        *vm.Manager
	Prov       *vm.Provisioner
	Runner     *exec.Runner
	Containers *container.Manager
	MicroVMs   *microvm.Manager
	Audit      *audit.Logger
}

// Register adds every Orbit tool to the server.
func Register(server *mcp.Server, d *Deps) {
	registerVMTools(server, d)
	registerKeyTools(server, d)
	registerExecTools(server, d)
	registerCredTools(server, d)
	registerContainerTools(server, d)
	registerMicroVMTools(server, d)
	registerStateTools(server, d)
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
