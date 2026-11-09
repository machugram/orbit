package tools

import (
	"context"
	"fmt"

	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/container"
	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/microvm"
	"github.com/machugram/orbit/internal/vm"
)

type runArgs struct {
	Command        string            `json:"command,omitempty" jsonschema:"shell command"`
	Script         string            `json:"script,omitempty" jsonschema:"script body"`
	Interpreter    string            `json:"interpreter,omitempty" jsonschema:"runs script; default bash on a vm, sh in a container"`
	Target         string            `json:"target,omitempty" jsonschema:"workspace to reuse, created if missing"`
	Targets        []string          `json:"targets,omitempty" jsonschema:"existing workspaces for this command"`
	Isolation      string            `json:"isolation,omitempty" jsonschema:"container, vm, or microvm"`
	Image          string            `json:"image,omitempty" jsonschema:"container or microvm image; default alpine"`
	Distro         string            `json:"distro,omitempty" jsonschema:"vm distro; default ubuntu"`
	Carrier        string            `json:"carrier,omitempty" jsonschema:"vm that hosts a microvm"`
	SecretEnv      map[string]string `json:"secret_env,omitempty" jsonschema:"env var name to vault secret name"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"kill after this many seconds; default 120, or 600 for a script"`
}

type commandOutcome struct {
	Target   string `json:"target,omitempty"`
	Kind     string `json:"kind"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
	Created  bool   `json:"created,omitempty"`
	Removed  bool   `json:"removed,omitempty"`
	Error    string `json:"error,omitempty"`
}

func registerRun(server *mcp.Server, d *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "run",
		Description: "Run a command or script. Returns stdout, stderr, and exit code. " +
			"No target: throwaway container, removed after. isolation=vm (~1 min) or microvm (minutes, needs carrier) when the work needs a machine or its own kernel, then removed. " +
			"target keeps the workspace. targets runs one command on existing workspaces. secret_env injects vault secrets by name.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args runArgs) (*mcp.CallToolResult, any, error) {
		out, err := d.run(ctx, args)
		d.log("run", map[string]any{
			"target": args.Target, "targets": args.Targets, "isolation": args.Isolation,
			"command": args.Command, "script_bytes": len(args.Script),
			"secret_env": envNames(args.SecretEnv),
		}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(out), nil, nil
	})
}

func (d *Deps) run(ctx context.Context, args runArgs) (any, error) {
	if err := checkInterpreter(args.Interpreter); err != nil {
		return nil, err
	}
	if args.Command == "" && args.Script == "" {
		return nil, fmt.Errorf("run needs a command or a script")
	}
	if args.Command != "" && args.Script != "" {
		return nil, fmt.Errorf("pass a command or a script, not both")
	}
	if args.Target != "" && len(args.Targets) > 0 {
		return nil, fmt.Errorf("pass target or targets, not both")
	}
	if len(args.Targets) > 0 {
		if args.Script != "" {
			return nil, fmt.Errorf("targets runs a command, not a script")
		}
		return d.runMany(ctx, args)
	}
	if _, err := normalizeKind(args.Isolation); err != nil {
		return nil, err
	}
	kind, _ := normalizeKind(args.Isolation)

	defTimeout := 120
	if args.Script != "" {
		defTimeout = 600
	}
	if args.Target == "" {
		return d.runEphemeral(ctx, args, kind, defTimeout)
	}

	// An empty isolation matches a workspace that already exists and
	// otherwise creates a container. A set isolation must match.
	view, err := d.openWorkspace(ctx, workspaceOpenArgs{
		Name:    args.Target,
		Kind:    args.Isolation,
		Distro:  args.Distro,
		Image:   args.Image,
		Carrier: args.Carrier,
	})
	if err != nil {
		return nil, err
	}
	runCtx, cancel := withTimeout(ctx, args.TimeoutSeconds, defTimeout)
	defer cancel()
	out, err := d.execIn(runCtx, view.Name, view.Kind, args)
	if err != nil {
		return nil, err
	}
	out.Created = view.Created
	return out, nil
}

func (d *Deps) runEphemeral(ctx context.Context, args runArgs, kind string, defTimeout int) (commandOutcome, error) {
	switch kind {
	case kindContainer:
		image := args.Image
		if image == "" {
			image = defaultImage
		}
		runCtx, cancel := withTimeout(ctx, args.TimeoutSeconds, defTimeout)
		defer cancel()
		spec := container.RunSpec{
			Image: image, Command: args.Command, SecretEnv: args.SecretEnv,
		}
		var res *container.Result
		var err error
		if args.Script != "" {
			res, err = d.Containers.RunScriptOnce(runCtx, spec, args.Interpreter, args.Script)
		} else {
			res, err = d.Containers.RunOnce(runCtx, spec)
		}
		if err != nil {
			return commandOutcome{}, err
		}
		return commandOutcome{
			Kind: kindContainer, Stdout: res.Stdout, Stderr: res.Stderr,
			ExitCode: res.ExitCode, Removed: true,
		}, nil
	case kindVM:
		distro := args.Distro
		if distro == "" {
			distro = defaultDistro
		}
		rec, err := d.Prov.Create(ctx, distro, "", true, vm.CreateOptions{Ephemeral: true})
		if err != nil {
			return commandOutcome{}, err
		}
		defer d.destroyQuiet(rec.ID, kindVM)
		runCtx, cancel := withTimeout(ctx, args.TimeoutSeconds, defTimeout)
		defer cancel()
		out, err := d.execIn(runCtx, rec.ID, kindVM, args)
		if err != nil {
			return commandOutcome{}, err
		}
		out.Removed = true
		return out, nil
	default:
		if args.Carrier == "" {
			return commandOutcome{}, fmt.Errorf("a microvm needs a carrier VM. workspace_open a vm first, then pass its name as carrier. A microvm takes minutes because it is emulated")
		}
		image := args.Image
		if image == "" {
			image = defaultImage
		}
		rec, err := d.MicroVMs.Launch(ctx, microvm.Spec{Carrier: args.Carrier, Image: image})
		if err != nil {
			return commandOutcome{}, err
		}
		defer d.destroyQuiet(rec.ID, kindMicroVM)
		runCtx, cancel := withTimeout(ctx, args.TimeoutSeconds, defTimeout)
		defer cancel()
		out, err := d.execIn(runCtx, rec.ID, kindMicroVM, args)
		if err != nil {
			return commandOutcome{}, err
		}
		out.Removed = true
		return out, nil
	}
}

func (d *Deps) runMany(ctx context.Context, args runArgs) (any, error) {
	for _, name := range args.Targets {
		_, ok, err := d.findWorkspace(ctx, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missingWorkspace(name)
		}
	}
	runCtx, cancel := withTimeout(ctx, args.TimeoutSeconds, 300)
	defer cancel()
	const limit = 8
	out := make([]commandOutcome, len(args.Targets))
	sem := make(chan struct{}, limit)
	errc := make(chan error, len(args.Targets))
	for i, name := range args.Targets {
		sem <- struct{}{}
		go func(i int, name string) {
			defer func() { <-sem }()
			kind, _, err := d.findWorkspace(runCtx, name)
			if err != nil {
				errc <- err
				return
			}
			if err := d.startWorkspace(runCtx, name, kind); err != nil {
				out[i] = commandOutcome{Target: name, Kind: kind, Error: err.Error()}
				errc <- nil
				return
			}
			res, err := d.execIn(runCtx, name, kind, args)
			if err != nil {
				out[i] = commandOutcome{Target: name, Kind: kind, Error: err.Error()}
			} else {
				out[i] = res
			}
			errc <- nil
		}(i, name)
	}
	var first error
	for range args.Targets {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return nil, first
	}
	return map[string]any{"results": out}, nil
}

func (d *Deps) execIn(ctx context.Context, name, kind string, args runArgs) (commandOutcome, error) {
	out := commandOutcome{Target: name, Kind: kind}
	switch kind {
	case kindVM, kindMicroVM:
		env, err := resolveExecSecretEnv(d, "", args.SecretEnv)
		if err != nil {
			return commandOutcome{}, err
		}
		var res *exec.Result
		if args.Script != "" {
			res, err = d.Runner.ExecScriptWithEnv(ctx, name, args.Script, args.Interpreter, env)
		} else {
			res, err = d.Runner.ExecWithEnv(ctx, name, args.Command, env)
		}
		if err != nil {
			return commandOutcome{}, err
		}
		out.Stdout, out.Stderr, out.ExitCode = res.Stdout, res.Stderr, res.ExitCode
	default:
		var res *container.Result
		var err error
		if args.Script != "" {
			res, err = d.Containers.ExecScript(ctx, name, args.Script, args.Interpreter, "", args.SecretEnv)
		} else {
			res, err = d.Containers.ExecCommand(ctx, name, args.Command, "", args.SecretEnv)
		}
		if err != nil {
			return commandOutcome{}, err
		}
		out.Stdout, out.Stderr, out.ExitCode = res.Stdout, res.Stderr, res.ExitCode
	}
	return out, nil
}

func (d *Deps) destroyQuiet(name, kind string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch kind {
	case kindVM:
		_ = d.VMs.Destroy(ctx, name, true)
	case kindMicroVM:
		_ = d.MicroVMs.Destroy(ctx, name, true)
	}
	if d.Runner != nil {
		d.Runner.Invalidate(name)
	}
}
