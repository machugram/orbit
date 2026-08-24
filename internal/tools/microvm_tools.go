package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/microvm"
	"github.com/machugram/orbit/internal/vault"
)

type microvmCreateArgs struct {
	Carrier   string `json:"carrier" jsonschema:"name of an existing running VM (from vm_create) that will host the nested guest"`
	Name      string `json:"name,omitempty" jsonschema:"microVM name; generated if omitted"`
	Image     string `json:"image,omitempty" jsonschema:"guest image: 'alpine' (default; light direct-kernel boot, tens of seconds, kernel may lack BTF) or 'ubuntu' (full cloud image, BTF kernel for CO-RE eBPF, minutes to boot)"`
	ImageURL  string `json:"image_url,omitempty" jsonschema:"override the guest cloud-image download URL (advanced/offline use)"`
	MemoryMiB int    `json:"memory_mib,omitempty" jsonschema:"guest memory in MiB (default 2048)"`
	CPUs      int    `json:"cpus,omitempty" jsonschema:"guest vCPUs (default 2)"`
}

type microvmIDArgs struct {
	Name string `json:"name" jsonschema:"microVM name as returned by microvm_create/microvm_list"`
}

type microvmDestroyArgs struct {
	Name     string `json:"name" jsonschema:"microVM name"`
	PurgeKey bool   `json:"purge_key,omitempty" jsonschema:"also delete the guest's SSH keypair from the vault"`
}

type microvmView struct {
	Name    string `json:"name"`
	Carrier string `json:"carrier"`
	Image   string `json:"image"`
	State   string `json:"state"`
	SSHHost string `json:"ssh_host,omitempty"`
	SSHPort int    `json:"ssh_port,omitempty"`
	KeyID   string `json:"key_id,omitempty"`
}

func microvmViewOf(mv *vault.MicroVM) microvmView {
	v := microvmView{
		Name: mv.ID, Carrier: mv.Carrier, Image: mv.Image,
		State: string(mv.State), KeyID: mv.KeyID,
	}
	if mv.State == vault.VMRunning {
		v.SSHHost = mv.Carrier + ".orb.local"
		v.SSHPort = mv.SSHPort
	}
	return v
}

func registerMicroVMTools(server *mcp.Server, d *Deps) {
	if d.MicroVMs == nil {
		return
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "microvm_create",
		Description: "Boot a nested QEMU guest inside a carrier VM. The guest runs its OWN kernel under software emulation (TCG), so eBPF programs loaded inside it cannot reach the shared OrbStack kernel or the host — use this for eBPF and other kernel-level work. The default 'alpine' image boots light (tens of seconds); 'ubuntu' is heavier but has a BTF kernel for CO-RE eBPF. The emulated CPU is slow either way. Once running, drive it with the normal exec/exec_script/file_push/file_pull tools using the microVM name as vm_id.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args microvmCreateArgs) (*mcp.CallToolResult, any, error) {
		rec, err := d.MicroVMs.Launch(ctx, microvm.Spec{
			Name:      args.Name,
			Carrier:   args.Carrier,
			Image:     args.Image,
			ImageURL:  args.ImageURL,
			MemoryMiB: args.MemoryMiB,
			CPUs:      args.CPUs,
		})
		d.log("microvm_create", map[string]any{
			"carrier": args.Carrier, "name": args.Name, "image": args.Image,
		}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(microvmViewOf(rec)), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "microvm_list",
		Description: "List all nested microVMs with their carriers, images, and states.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		recs, err := d.MicroVMs.List()
		d.log("microvm_list", nil, err)
		if err != nil {
			return nil, nil, err
		}
		views := make([]microvmView, len(recs))
		for i := range recs {
			views[i] = microvmViewOf(&recs[i])
		}
		return jsonResult(views), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "microvm_start",
		Description: "Reboot a stopped microVM from its staged disk (no re-download). Blocks until it accepts SSH.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args microvmIDArgs) (*mcp.CallToolResult, any, error) {
		rec, err := d.MicroVMs.Start(ctx, args.Name)
		d.log("microvm_start", map[string]any{"name": args.Name}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(microvmViewOf(rec)), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "microvm_stop",
		Description: "Stop a running microVM's QEMU process, leaving its disk staged for a later microvm_start.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args microvmIDArgs) (*mcp.CallToolResult, any, error) {
		err := d.MicroVMs.Stop(ctx, args.Name)
		if d.Runner != nil {
			d.Runner.Invalidate(args.Name)
		}
		d.log("microvm_stop", map[string]any{"name": args.Name}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"name": args.Name, "state": "stopped"}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "microvm_destroy",
		Description: "Destroy a microVM: kill its QEMU process and remove its working directory on the carrier, optionally purging its SSH keypair.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args microvmDestroyArgs) (*mcp.CallToolResult, any, error) {
		err := d.MicroVMs.Destroy(ctx, args.Name, args.PurgeKey)
		if d.Runner != nil {
			d.Runner.Invalidate(args.Name)
		}
		d.log("microvm_destroy", map[string]any{"name": args.Name, "purge_key": args.PurgeKey}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"name": args.Name, "state": "destroyed", "key_purged": args.PurgeKey}), nil, nil
	})
}
