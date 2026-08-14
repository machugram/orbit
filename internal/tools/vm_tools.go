package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/vault"
)

type vmCreateArgs struct {
	Distro   string `json:"distro" jsonschema:"OrbStack distro, optionally versioned (ubuntu, ubuntu:24.04, alpine, fedora, ...)"`
	Name     string `json:"name,omitempty" jsonschema:"machine name; generated if omitted"`
	Isolated *bool  `json:"isolated,omitempty" jsonschema:"create in OrbStack isolated mode (default true; no macOS file sharing)"`
}

type vmIDArgs struct {
	VMID string `json:"vm_id" jsonschema:"machine name as returned by vm_create/vm_list"`
}

type vmDestroyArgs struct {
	VMID      string `json:"vm_id" jsonschema:"machine name"`
	PurgeKeys bool   `json:"purge_keys,omitempty" jsonschema:"also delete the VM's SSH keypair from the vault"`
}

type vmView struct {
	VMID    string `json:"vm_id"`
	Distro  string `json:"distro"`
	State   string `json:"state"`
	KeyID   string `json:"key_id,omitempty"`
	SSHUser string `json:"ssh_user,omitempty"`
}

func viewOf(rec *vault.VM, sshUser string) vmView {
	v := vmView{VMID: rec.ID, Distro: rec.Distro, State: string(rec.State), KeyID: rec.KeyID}
	if rec.State == vault.VMRunning {
		v.SSHUser = sshUser
	}
	return v
}

func registerVMTools(server *mcp.Server, d *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "vm_create",
		Description: "Create an isolated OrbStack Linux VM provisioned with a freshly generated SSH keypair owned by the agent. Blocks until the VM is running.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args vmCreateArgs) (*mcp.CallToolResult, any, error) {
		isolated := args.Isolated == nil || *args.Isolated
		rec, err := d.Prov.Create(ctx, args.Distro, args.Name, isolated)
		d.log("vm_create", map[string]any{"distro": args.Distro, "name": args.Name, "isolated": isolated}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(viewOf(rec, d.Prov.User())), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "vm_list",
		Description: "List all agent-managed VMs with their states (refreshed from OrbStack when available).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		vms, err := d.VMs.List(ctx)
		d.log("vm_list", nil, err)
		if err != nil {
			return nil, nil, err
		}
		views := make([]vmView, len(vms))
		for i := range vms {
			views[i] = viewOf(&vms[i], d.Prov.User())
		}
		return jsonResult(views), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "vm_start",
		Description: "Start a stopped agent-managed VM.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args vmIDArgs) (*mcp.CallToolResult, any, error) {
		err := d.VMs.Start(ctx, args.VMID)
		d.log("vm_start", map[string]any{"vm_id": args.VMID}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"vm_id": args.VMID, "state": "running"}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "vm_stop",
		Description: "Gracefully stop an agent-managed VM.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args vmIDArgs) (*mcp.CallToolResult, any, error) {
		err := d.VMs.Stop(ctx, args.VMID)
		d.log("vm_stop", map[string]any{"vm_id": args.VMID}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"vm_id": args.VMID, "state": "stopped"}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "vm_destroy",
		Description: "Destroy an agent-managed VM, optionally purging its SSH keypair from the vault.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args vmDestroyArgs) (*mcp.CallToolResult, any, error) {
		err := d.VMs.Destroy(ctx, args.VMID, args.PurgeKeys)
		d.log("vm_destroy", map[string]any{"vm_id": args.VMID, "purge_keys": args.PurgeKeys}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"vm_id": args.VMID, "state": "destroyed", "keys_purged": args.PurgeKeys}), nil, nil
	})
}
