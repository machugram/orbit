package tools

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/state"
)

type stateArgs struct {
	// State is the desired-state JSON document. Must be a JSON string.
	State json.RawMessage `json:"state" jsonschema:"desired state as JSON: {vms:[...], containers:[...], composes:[...]}"`
}

func registerStateTools(server *mcp.Server, d *Deps) {
	r := state.NewReconciler(d.VMs, d.Prov, d.Containers)

	mcp.AddTool(server, &mcp.Tool{
		Name: "state_plan",
		Description: "Dry-run: diff the desired state against live Orbit resources and return the list of actions that state_apply would take — without executing any of them. " +
			"State JSON fields: vms (name+distro), containers (image, name, ports, env), composes (project, yaml or file).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args stateArgs) (*mcp.CallToolResult, any, error) {
		desired, err := state.ParseState(args.State)
		if err != nil {
			return nil, nil, err
		}
		plan, err := r.Plan(ctx, desired)
		d.log("state_plan", map[string]any{"vms": len(desired.VMs), "containers": len(desired.Containers), "composes": len(desired.Composes)}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(plan), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "state_apply",
		Description: "Apply a desired state: create VMs, run containers, and bring up compose stacks that are missing. Already-running resources are left untouched. " +
			"State JSON fields: vms (name+distro), containers (image, name, ports, env), composes (project, yaml or file).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args stateArgs) (*mcp.CallToolResult, any, error) {
		desired, err := state.ParseState(args.State)
		if err != nil {
			return nil, nil, err
		}
		result, err := r.Apply(ctx, desired)
		d.log("state_apply", map[string]any{"vms": len(desired.VMs), "containers": len(desired.Containers), "composes": len(desired.Composes)}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(result), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "state_destroy",
		Description: "Destroy all resources named in the state document: stops and removes containers, tears down compose stacks, and destroys VMs (purging their keys). " +
			"Only resources listed in the document are touched.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args stateArgs) (*mcp.CallToolResult, any, error) {
		desired, err := state.ParseState(args.State)
		if err != nil {
			return nil, nil, err
		}
		result, err := r.Destroy(ctx, desired)
		d.log("state_destroy", map[string]any{"vms": len(desired.VMs), "containers": len(desired.Containers), "composes": len(desired.Composes)}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(result), nil, nil
	})
}
