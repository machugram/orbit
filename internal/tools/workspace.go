package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/container"
	"github.com/machugram/orbit/internal/microvm"
	"github.com/machugram/orbit/internal/vault"
	"github.com/machugram/orbit/internal/vm"
)

type workspaceOpenArgs struct {
	Name      string `json:"name,omitempty" jsonschema:"generated if omitted"`
	Kind      string `json:"kind,omitempty" jsonschema:"container, vm, or microvm"`
	Distro    string `json:"distro,omitempty" jsonschema:"vm distro; default ubuntu"`
	Image     string `json:"image,omitempty" jsonschema:"container or microvm image; default alpine"`
	Carrier   string `json:"carrier,omitempty" jsonschema:"vm that hosts a microvm"`
	Ephemeral bool   `json:"ephemeral,omitempty" jsonschema:"vm only: destroy after 30 minutes idle"`
}

type workspaceCloseArgs struct {
	Name string `json:"name" jsonschema:"workspace name"`
}

type workspaceView struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Distro    string `json:"distro,omitempty"`
	Image     string `json:"image,omitempty"`
	SSHUser   string `json:"ssh_user,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
	Host      string `json:"host,omitempty"`
	Carrier   string `json:"carrier,omitempty"`
	Created   bool   `json:"created"`
}

func registerWorkspace(server *mcp.Server, d *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "workspace_open",
		Description: "Open a container (seconds), vm (~1 min, for untrusted code), or microvm (minutes, own kernel, needs carrier). " +
			"Returns name, address, and SSH public key. Starts an existing workspace if it is stopped. Private keys stay in the vault. Use service_up to publish ports.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args workspaceOpenArgs) (*mcp.CallToolResult, any, error) {
		view, err := d.openWorkspace(ctx, args)
		d.log("workspace_open", map[string]any{
			"name": args.Name, "kind": args.Kind, "distro": args.Distro,
			"image": args.Image, "carrier": args.Carrier, "ephemeral": args.Ephemeral,
		}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(view), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "workspace_close",
		Description: "Destroy a workspace and delete its SSH key. A compose project is removed with service_down.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args workspaceCloseArgs) (*mcp.CallToolResult, any, error) {
		view, err := d.closeWorkspace(ctx, args.Name)
		d.log("workspace_close", map[string]any{"name": args.Name}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(view), nil, nil
	})
}

func (d *Deps) openWorkspace(ctx context.Context, args workspaceOpenArgs) (workspaceView, error) {
	kind, err := normalizeKind(args.Kind)
	if err != nil {
		return workspaceView{}, err
	}
	name := args.Name
	if name == "" {
		switch kind {
		case kindVM:
			name = ""
		case kindMicroVM:
			name = ""
		default:
			name = newName("box-")
		}
	}
	if name != "" {
		if err := checkName(name); err != nil {
			return workspaceView{}, err
		}
		var found string
		var ok bool
		if kind == kindContainer {
			found, ok, err = d.findWorkspace(ctx, name)
		} else {
			found, ok, err = d.findMachine(name)
		}
		if err != nil {
			return workspaceView{}, err
		}
		if ok {
			if args.Kind != "" && found != kind {
				return workspaceView{}, fmt.Errorf("workspace %q is a %s, not a %s", name, found, kind)
			}
			if err := d.startWorkspace(ctx, name, found); err != nil {
				return workspaceView{}, err
			}
			return d.workspaceView(ctx, name, found, false)
		}
	}

	switch kind {
	case kindVM:
		return d.createVMWorkspace(ctx, name, args)
	case kindMicroVM:
		return d.createMicroVMWorkspace(ctx, name, args)
	default:
		return d.createContainerWorkspace(ctx, name, args)
	}
}

func (d *Deps) createVMWorkspace(ctx context.Context, name string, args workspaceOpenArgs) (workspaceView, error) {
	distro := args.Distro
	if distro == "" {
		distro = defaultDistro
	}
	opt := vm.CreateOptions{Ephemeral: args.Ephemeral}
	rec, err := d.Prov.Create(ctx, distro, name, true, opt)
	if err != nil {
		return workspaceView{}, err
	}
	return d.workspaceView(ctx, rec.ID, kindVM, true)
}

func (d *Deps) createMicroVMWorkspace(ctx context.Context, name string, args workspaceOpenArgs) (workspaceView, error) {
	if args.Carrier == "" {
		return workspaceView{}, fmt.Errorf("a microvm needs a carrier VM. workspace_open a vm first, then pass its name as carrier. A microvm takes minutes because it is emulated")
	}
	image := args.Image
	if image == "" {
		image = defaultImage
	}
	rec, err := d.MicroVMs.Launch(ctx, microvm.Spec{
		Name:    name,
		Carrier: args.Carrier,
		Image:   image,
	})
	if err != nil {
		return workspaceView{}, err
	}
	return d.workspaceView(ctx, rec.ID, kindMicroVM, true)
}

func (d *Deps) createContainerWorkspace(ctx context.Context, name string, args workspaceOpenArgs) (workspaceView, error) {
	image := args.Image
	if image == "" {
		image = defaultImage
	}
	if _, err := d.Containers.Run(ctx, container.RunSpec{
		Image:   image,
		Name:    name,
		Command: "sleep infinity",
		Detach:  true,
	}); err != nil {
		return workspaceView{}, err
	}
	return workspaceView{
		Name:    name,
		Kind:    kindContainer,
		State:   "running",
		Image:   image,
		Host:    name + ".orb.local",
		Created: true,
	}, nil
}

func (d *Deps) workspaceView(ctx context.Context, name, kind string, created bool) (workspaceView, error) {
	view := workspaceView{
		Name:      name,
		Kind:      kind,
		Created:   created,
		PublicKey: d.publicKey(kind, name),
	}
	switch kind {
	case kindVM:
		rec, err := d.Vault.GetVM(name)
		if err != nil {
			return workspaceView{}, err
		}
		view.State = string(rec.State)
		view.Distro = rec.Distro
		if rec.State == vault.VMRunning {
			view.SSHUser = d.Prov.User()
			view.Host = name + ".orb.local"
		}
	case kindMicroVM:
		mv, err := d.MicroVMs.Get(name)
		if err != nil {
			return workspaceView{}, err
		}
		view.State = string(mv.State)
		view.Image = mv.Image
		view.Carrier = mv.Carrier
		if mv.State == vault.VMRunning {
			view.SSHUser = d.Prov.User()
			view.Host = fmt.Sprintf("%s.orb.local:%d", mv.Carrier, mv.SSHPort)
		}
	default:
		view.State = "running"
		view.Host = name + ".orb.local"
	}
	return view, nil
}

func (d *Deps) closeWorkspace(ctx context.Context, name string) (workspaceView, error) {
	kind, ok, err := d.findWorkspace(ctx, name)
	if err != nil {
		return workspaceView{}, err
	}
	if !ok {
		return workspaceView{}, missingWorkspace(name)
	}
	switch kind {
	case kindVM:
		if err := d.VMs.Destroy(ctx, name, true); err != nil {
			return workspaceView{}, err
		}
		if d.Runner != nil {
			d.Runner.Invalidate(name)
		}
	case kindMicroVM:
		if err := d.MicroVMs.Destroy(ctx, name, true); err != nil {
			return workspaceView{}, err
		}
		if d.Runner != nil {
			d.Runner.Invalidate(name)
		}
	default:
		if err := d.Containers.Remove(ctx, name, true); err != nil {
			return workspaceView{}, err
		}
	}
	return workspaceView{Name: name, Kind: kind, State: "destroyed"}, nil
}
