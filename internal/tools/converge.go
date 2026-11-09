package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/state"
)

type convergeArgs struct {
	State state.State `json:"state" jsonschema:"vms, containers, and compose stacks that should exist"`
}

func registerConverge(server *mcp.Server, d *Deps) {
	r := state.NewReconciler(d.VMs, d.Prov, d.Containers)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "converge",
		Description: "Create VMs, containers, and compose stacks named in state that are missing. Does not delete. See orbit://workspaces.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args convergeArgs) (*mcp.CallToolResult, any, error) {
		desired := args.State
		if err := desired.Validate(); err != nil {
			d.log("converge", nil, err)
			return nil, nil, err
		}
		result, err := r.Apply(ctx, &desired)
		d.log("converge", map[string]any{
			"vms": len(desired.VMs), "containers": len(desired.Containers), "composes": len(desired.Composes),
		}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(result), nil, nil
	})
}
