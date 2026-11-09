package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/container"
)

type serviceUpArgs struct {
	Image     string            `json:"image,omitempty" jsonschema:"one container; not with project"`
	Name      string            `json:"name,omitempty" jsonschema:"container name; generated if omitted"`
	Command   string            `json:"command,omitempty" jsonschema:"replaces the image command"`
	Ports     []string          `json:"ports,omitempty" jsonschema:"for example 8080:80"`
	Env       map[string]string `json:"env,omitempty" jsonschema:"literal environment variables"`
	Project   string            `json:"project,omitempty" jsonschema:"compose project; not with image"`
	File      string            `json:"file,omitempty" jsonschema:"compose file; not with yaml"`
	Yaml      string            `json:"yaml,omitempty" jsonschema:"inline compose YAML; not with file"`
	Build     bool              `json:"build,omitempty" jsonschema:"build compose images first"`
	SecretEnv map[string]string `json:"secret_env,omitempty" jsonschema:"env var name to vault secret name"`
}

type serviceDownArgs struct {
	Name    string `json:"name,omitempty" jsonschema:"container to remove"`
	Project string `json:"project,omitempty" jsonschema:"compose project to remove"`
}

type endpoint struct {
	Service string `json:"service,omitempty"`
	Name    string `json:"name"`
	Host    string `json:"host,omitempty"`
	State   string `json:"state,omitempty"`
	Health  string `json:"health,omitempty"`
	Ports   string `json:"ports,omitempty"`
}

type serviceView struct {
	Kind      string     `json:"kind"`
	Name      string     `json:"name,omitempty"`
	Project   string     `json:"project,omitempty"`
	Host      string     `json:"host,omitempty"`
	Image     string     `json:"image,omitempty"`
	Ports     []string   `json:"ports,omitempty"`
	Endpoints []endpoint `json:"endpoints,omitempty"`
	Logs      string     `json:"logs,omitempty"`
}

func registerService(server *mcp.Server, d *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "service_up",
		Description: "Start a container (image) or compose stack (project plus yaml or file) and return its address. " +
			"Ready in seconds; shares the OrbStack kernel. secret_env injects vault secrets by name. The endpoint name is the target for run and file_put.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args serviceUpArgs) (*mcp.CallToolResult, any, error) {
		view, err := d.serviceUp(ctx, args)
		d.log("service_up", map[string]any{
			"image": args.Image, "name": args.Name, "project": args.Project,
			"secret_env": envNames(args.SecretEnv), "build": args.Build,
		}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(view), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "service_down",
		Description: "Stop a container (name) or compose project (project).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args serviceDownArgs) (*mcp.CallToolResult, any, error) {
		view, err := d.serviceDown(ctx, args)
		d.log("service_down", map[string]any{"name": args.Name, "project": args.Project}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(view), nil, nil
	})
}

func (d *Deps) serviceUp(ctx context.Context, args serviceUpArgs) (serviceView, error) {
	if args.Image != "" && args.Project != "" {
		return serviceView{}, fmt.Errorf("bring up one container (image) or one compose project (project), not both")
	}
	if args.Image == "" && args.Project == "" {
		return serviceView{}, fmt.Errorf("service_up needs an image or a project")
	}
	if args.Image != "" {
		name := args.Name
		if name == "" {
			name = newName("svc-")
		}
		if err := checkName(name); err != nil {
			return serviceView{}, err
		}
		if _, err := d.Containers.Run(ctx, container.RunSpec{
			Image:     args.Image,
			Name:      name,
			Command:   args.Command,
			Env:       args.Env,
			SecretEnv: args.SecretEnv,
			Ports:     args.Ports,
			Detach:    true,
		}); err != nil {
			return serviceView{}, err
		}
		view := serviceView{
			Kind: kindContainer, Name: name, Image: args.Image,
			Host: name + ".orb.local", Ports: args.Ports,
		}
		if logs, err := d.Containers.Logs(ctx, name, 40); err == nil && strings.TrimSpace(logs) != "" {
			// Logs are useful when the process exits immediately. A healthy
			// detached server often has none yet; include them only then.
			list, lerr := d.Containers.List(ctx, true)
			if lerr == nil {
				for _, c := range list {
					if strings.TrimPrefix(c.Name, "/") == name && c.State != "running" && !strings.HasPrefix(c.State, "Up") {
						view.Logs = logs
					}
				}
			}
		}
		return view, nil
	}

	spec := container.ComposeSpec{
		Project:   args.Project,
		File:      args.File,
		YAML:      args.Yaml,
		Env:       args.Env,
		SecretEnv: args.SecretEnv,
	}
	if args.Build {
		if _, err := d.Containers.ComposeBuild(ctx, spec, nil); err != nil {
			return serviceView{}, err
		}
	}
	if _, err := d.Containers.ComposeUp(ctx, spec); err != nil {
		return serviceView{}, err
	}
	services, err := d.Containers.ComposePs(ctx, spec)
	if err != nil {
		return serviceView{}, err
	}
	view := serviceView{Kind: "compose", Project: args.Project}
	unhealthy := false
	for _, s := range services {
		view.Endpoints = append(view.Endpoints, endpoint{
			Service: s.Service, Name: s.Name, Host: s.OrbHost,
			State: s.State, Health: s.Health, Ports: s.Ports,
		})
		if s.State != "running" && !strings.HasPrefix(s.State, "Up") {
			unhealthy = true
		}
	}
	if unhealthy {
		if logs, err := d.Containers.ComposeLogs(ctx, spec, 40); err == nil {
			view.Logs = logs
		}
	}
	return view, nil
}

func (d *Deps) serviceDown(ctx context.Context, args serviceDownArgs) (serviceView, error) {
	if args.Name != "" && args.Project != "" {
		return serviceView{}, fmt.Errorf("pass name or project, not both")
	}
	if args.Name == "" && args.Project == "" {
		return serviceView{}, fmt.Errorf("service_down needs a name or a project")
	}
	if args.Project != "" {
		if err := d.Containers.ComposeDownProject(ctx, args.Project); err != nil {
			return serviceView{}, err
		}
		return serviceView{Kind: "compose", Project: args.Project, Name: args.Project}, nil
	}
	if err := checkName(args.Name); err != nil {
		return serviceView{}, err
	}
	if err := d.Containers.Remove(ctx, args.Name, true); err != nil {
		return serviceView{}, err
	}
	return serviceView{Kind: kindContainer, Name: args.Name}, nil
}
