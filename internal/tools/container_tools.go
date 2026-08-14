package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/container"
)

type containerRunArgs struct {
	Image     string            `json:"image" jsonschema:"image to run, e.g. alpine or nginx:1.27"`
	Name      string            `json:"name,omitempty" jsonschema:"container name; docker generates one if omitted"`
	Command   string            `json:"command,omitempty" jsonschema:"shell command to run instead of the image's default (run via sh -c)"`
	Env       map[string]string `json:"env,omitempty" jsonschema:"literal environment variables (NAME->value)"`
	SecretEnv map[string]string `json:"secret_env,omitempty" jsonschema:"environment variable NAME -> vault secret key; the value is read from the vault and injected without passing through the model"`
	Namespace string            `json:"namespace,omitempty" jsonschema:"vault namespace for secret_env (default 'default')"`
	Ports     []string          `json:"ports,omitempty" jsonschema:"port mappings, e.g. 8080:80 or 127.0.0.1:5432:5432"`
	Detach    *bool             `json:"detach,omitempty" jsonschema:"run detached in the background (default true)"`
	Remove    bool              `json:"remove,omitempty" jsonschema:"remove the container when it exits (only when not detached)"`
}

type containerExecArgs struct {
	Name    string `json:"name" jsonschema:"container name or id"`
	Command string `json:"command" jsonschema:"shell command to run inside the container"`
}

type containerLogsArgs struct {
	Name string `json:"name" jsonschema:"container name or id"`
	Tail int    `json:"tail,omitempty" jsonschema:"return only the last N lines (default: all)"`
}

type containerNameArgs struct {
	Name  string `json:"name" jsonschema:"container name or id"`
	Force bool   `json:"force,omitempty" jsonschema:"force removal of a running container"`
}

type containerListArgs struct {
	All bool `json:"all,omitempty" jsonschema:"include stopped containers (default: running only)"`
}

type composeArgs struct {
	Project   string            `json:"project" jsonschema:"compose project name (isolates this stack)"`
	File      string            `json:"file,omitempty" jsonschema:"path to a docker-compose file on the host"`
	Yaml      string            `json:"yaml,omitempty" jsonschema:"inline compose YAML (mutually exclusive with file)"`
	Env       map[string]string `json:"env,omitempty" jsonschema:"literal env for ${VAR} interpolation"`
	SecretEnv map[string]string `json:"secret_env,omitempty" jsonschema:"env var NAME -> vault secret key, injected from the vault"`
	Namespace string            `json:"namespace,omitempty" jsonschema:"vault namespace for secret_env (default 'default')"`
	Scale     map[string]int    `json:"scale,omitempty" jsonschema:"for compose_up: service name -> replica count"`
	Tail      int               `json:"tail,omitempty" jsonschema:"for compose_logs: last N lines (default: all)"`
}

type composeExecArgs struct {
	Project string `json:"project" jsonschema:"compose project name"`
	File    string `json:"file,omitempty" jsonschema:"path to the docker-compose file on the host"`
	Yaml    string `json:"yaml,omitempty" jsonschema:"inline compose YAML (mutually exclusive with file)"`
	Service string `json:"service" jsonschema:"service name to exec into (not the container name)"`
	Command string `json:"command" jsonschema:"shell command to run inside the service"`
}

type composeBuildArgs struct {
	Project  string   `json:"project" jsonschema:"compose project name"`
	File     string   `json:"file,omitempty" jsonschema:"path to the docker-compose file on the host"`
	Yaml     string   `json:"yaml,omitempty" jsonschema:"inline compose YAML (mutually exclusive with file)"`
	Services []string `json:"services,omitempty" jsonschema:"services to build (default: all defined in the project)"`
}

type imageBuildArgs struct {
	Tag         string            `json:"tag" jsonschema:"image tag to build, e.g. myapp:dev"`
	ContextPath string            `json:"context_path" jsonschema:"build context directory on the host"`
	Dockerfile  string            `json:"dockerfile,omitempty" jsonschema:"inline Dockerfile contents; if omitted, context_path/Dockerfile is used"`
	BuildArgs   map[string]string `json:"build_args,omitempty" jsonschema:"--build-arg values (NAME->value)"`
}

// secretKeys returns just the env var names referenced, for audit logging —
// never the vault keys' values.
func envNames(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}

func (cs *composeArgs) toSpec() container.ComposeSpec {
	return container.ComposeSpec{
		Project:   cs.Project,
		File:      cs.File,
		YAML:      cs.Yaml,
		Env:       cs.Env,
		SecretEnv: cs.SecretEnv,
		Namespace: cs.Namespace,
		Scale:     cs.Scale,
	}
}

func registerContainerTools(server *mcp.Server, d *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "container_run",
		Description: "Run a Docker container on OrbStack. Secrets named in secret_env are read from the vault and injected as environment variables without their values ever entering the model or the command line. Returns the container id.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args containerRunArgs) (*mcp.CallToolResult, any, error) {
		detach := args.Detach == nil || *args.Detach
		id, err := d.Containers.Run(ctx, container.RunSpec{
			Image:     args.Image,
			Name:      args.Name,
			Command:   args.Command,
			Env:       args.Env,
			SecretEnv: args.SecretEnv,
			Namespace: args.Namespace,
			Ports:     args.Ports,
			Detach:    detach,
			Remove:    args.Remove,
		})
		d.log("container_run", map[string]any{
			"image": args.Image, "name": args.Name,
			"env": envNames(args.Env), "secret_env": envNames(args.SecretEnv),
			"ports": args.Ports, "detach": detach,
		}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"container_id": id, "image": args.Image, "name": args.Name}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "container_exec",
		Description: "Run a shell command inside a running container. Non-zero exit codes are returned as results, not errors.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args containerExecArgs) (*mcp.CallToolResult, any, error) {
		res, err := d.Containers.Exec(ctx, args.Name, args.Command)
		d.log("container_exec", map[string]any{"name": args.Name, "command": args.Command}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(res), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "container_logs",
		Description: "Fetch logs from a container.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args containerLogsArgs) (*mcp.CallToolResult, any, error) {
		out, err := d.Containers.Logs(ctx, args.Name, args.Tail)
		d.log("container_logs", map[string]any{"name": args.Name, "tail": args.Tail}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"name": args.Name, "logs": out}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "container_list",
		Description: "List containers (running only by default; set all=true for stopped ones too).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args containerListArgs) (*mcp.CallToolResult, any, error) {
		list, err := d.Containers.List(ctx, args.All)
		d.log("container_list", map[string]any{"all": args.All}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(list), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "container_stop",
		Description: "Stop a running container.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args containerNameArgs) (*mcp.CallToolResult, any, error) {
		err := d.Containers.Stop(ctx, args.Name)
		d.log("container_stop", map[string]any{"name": args.Name}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"name": args.Name, "status": "stopped"}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "container_rm",
		Description: "Remove a container (set force=true to remove one that is still running).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args containerNameArgs) (*mcp.CallToolResult, any, error) {
		err := d.Containers.Remove(ctx, args.Name, args.Force)
		d.log("container_rm", map[string]any{"name": args.Name, "force": args.Force}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"name": args.Name, "status": "removed"}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "compose_up",
		Description: "Bring a docker-compose project up (detached) from a host file or inline YAML. Services share a network and resolve each other by name. Secrets named in secret_env are injected from the vault for ${VAR} interpolation; scale sets per-service replica counts.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args composeArgs) (*mcp.CallToolResult, any, error) {
		out, err := d.Containers.ComposeUp(ctx, args.toSpec())
		d.log("compose_up", map[string]any{
			"project": args.Project, "file": args.File, "inline_yaml": args.Yaml != "",
			"env": envNames(args.Env), "secret_env": envNames(args.SecretEnv), "scale": args.Scale,
		}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"project": args.Project, "status": "up", "output": out}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "compose_ps",
		Description: "List a compose project's services with their state, health, published ports, and the <name>.orb.local hostname each is reachable on from the host. Includes stopped/crashed services.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args composeArgs) (*mcp.CallToolResult, any, error) {
		services, err := d.Containers.ComposePs(ctx, args.toSpec())
		d.log("compose_ps", map[string]any{"project": args.Project}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"project": args.Project, "services": services}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "compose_exec",
		Description: "Run a shell command inside a project's service by service name (resolves the running container for you). Non-zero exit codes are returned as results, not errors.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args composeExecArgs) (*mcp.CallToolResult, any, error) {
		spec := container.ComposeSpec{Project: args.Project, File: args.File, YAML: args.Yaml}
		res, err := d.Containers.ComposeExec(ctx, spec, args.Service, args.Command)
		d.log("compose_exec", map[string]any{"project": args.Project, "service": args.Service, "command": args.Command}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(res), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "compose_build",
		Description: "Build images for a compose project (all services, or only the named ones).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args composeBuildArgs) (*mcp.CallToolResult, any, error) {
		spec := container.ComposeSpec{Project: args.Project, File: args.File, YAML: args.Yaml}
		out, err := d.Containers.ComposeBuild(ctx, spec, args.Services)
		d.log("compose_build", map[string]any{"project": args.Project, "services": args.Services}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"project": args.Project, "status": "built", "output": out}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "compose_down",
		Description: "Tear down a docker-compose project (stops and removes its containers and networks).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args composeArgs) (*mcp.CallToolResult, any, error) {
		err := d.Containers.ComposeDown(ctx, args.toSpec())
		d.log("compose_down", map[string]any{"project": args.Project}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"project": args.Project, "status": "down"}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "compose_logs",
		Description: "Fetch logs for a docker-compose project.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args composeArgs) (*mcp.CallToolResult, any, error) {
		out, err := d.Containers.ComposeLogs(ctx, args.toSpec(), args.Tail)
		d.log("compose_logs", map[string]any{"project": args.Project, "tail": args.Tail}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"project": args.Project, "logs": out}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "image_build",
		Description: "Build a Docker image from a host context directory, optionally with an inline Dockerfile.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args imageBuildArgs) (*mcp.CallToolResult, any, error) {
		out, err := d.Containers.Build(ctx, container.BuildSpec{
			Tag:         args.Tag,
			ContextPath: args.ContextPath,
			Dockerfile:  args.Dockerfile,
			BuildArgs:   args.BuildArgs,
		})
		d.log("image_build", map[string]any{
			"tag": args.Tag, "context_path": args.ContextPath,
			"inline_dockerfile": args.Dockerfile != "", "build_args": envNames(args.BuildArgs),
		}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"tag": args.Tag, "status": "built", "output": out}), nil, nil
	})
}
