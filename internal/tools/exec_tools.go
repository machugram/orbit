package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type execArgs struct {
	VMID           string `json:"vm_id" jsonschema:"machine name"`
	Command        string `json:"command" jsonschema:"shell command to run"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"kill the command after this many seconds (default 120)"`
}

type execScriptArgs struct {
	VMID           string `json:"vm_id" jsonschema:"machine name"`
	Script         string `json:"script" jsonschema:"script body to upload and run"`
	Interpreter    string `json:"interpreter,omitempty" jsonschema:"interpreter to run the script with (default bash)"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"kill the script after this many seconds (default 600)"`
}

type execParallelArgs struct {
	VMIDs          []string `json:"vm_ids" jsonschema:"machine names to run the command on"`
	Command        string   `json:"command" jsonschema:"shell command to run on every VM"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"overall deadline in seconds (default 300)"`
	MaxConcurrent  int      `json:"max_concurrent,omitempty" jsonschema:"max simultaneous SSH sessions (default 8)"`
}

type filePushArgs struct {
	VMID       string `json:"vm_id" jsonschema:"machine name"`
	LocalPath  string `json:"local_path" jsonschema:"path of the file on the host"`
	RemotePath string `json:"remote_path" jsonschema:"destination path inside the VM"`
}

type filePullArgs struct {
	VMID       string `json:"vm_id" jsonschema:"machine name"`
	RemotePath string `json:"remote_path" jsonschema:"path of the file inside the VM"`
	LocalPath  string `json:"local_path,omitempty" jsonschema:"write the file here on the host instead of returning its content"`
}

func withTimeout(ctx context.Context, seconds, def int) (context.Context, context.CancelFunc) {
	if seconds <= 0 {
		seconds = def
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}

func registerExecTools(server *mcp.Server, d *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "exec",
		Description: "Run a shell command on a VM over SSH. Returns stdout, stderr, and the exit code (non-zero exits are results, not errors).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args execArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withTimeout(ctx, args.TimeoutSeconds, 120)
		defer cancel()
		res, err := d.Runner.Exec(ctx, args.VMID, args.Command)
		d.log("exec", map[string]any{"vm_id": args.VMID, "command": args.Command}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(res), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "exec_script",
		Description: "Upload a script to a VM, run it with the given interpreter (default bash), and clean it up afterwards.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args execScriptArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withTimeout(ctx, args.TimeoutSeconds, 600)
		defer cancel()
		res, err := d.Runner.ExecScript(ctx, args.VMID, args.Script, args.Interpreter)
		d.log("exec_script", map[string]any{"vm_id": args.VMID, "interpreter": args.Interpreter, "script_bytes": len(args.Script)}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(res), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "exec_parallel",
		Description: "Run the same shell command on multiple VMs concurrently. Per-VM failures are reported per entry and do not cancel the rest.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args execParallelArgs) (*mcp.CallToolResult, any, error) {
		ctx, cancel := withTimeout(ctx, args.TimeoutSeconds, 300)
		defer cancel()
		results := d.Runner.ExecParallel(ctx, args.VMIDs, args.Command, args.MaxConcurrent)
		d.log("exec_parallel", map[string]any{"vm_ids": args.VMIDs, "command": args.Command}, nil)
		return jsonResult(results), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "file_push",
		Description: "Upload a file from the host into a VM (parent directories are created).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args filePushArgs) (*mcp.CallToolResult, any, error) {
		err := d.Runner.Push(ctx, args.VMID, args.LocalPath, args.RemotePath)
		d.log("file_push", map[string]any{"vm_id": args.VMID, "local_path": args.LocalPath, "remote_path": args.RemotePath}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"vm_id": args.VMID, "remote_path": args.RemotePath, "status": "pushed"}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "file_pull",
		Description: "Download a file from a VM. Returns text content directly, binary content base64-encoded, or writes to local_path if given. Capped at 32 MiB.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args filePullArgs) (*mcp.CallToolResult, any, error) {
		data, err := d.Runner.Pull(ctx, args.VMID, args.RemotePath)
		d.log("file_pull", map[string]any{"vm_id": args.VMID, "remote_path": args.RemotePath, "local_path": args.LocalPath}, err)
		if err != nil {
			return nil, nil, err
		}
		if args.LocalPath != "" {
			if err := os.WriteFile(args.LocalPath, data, 0o600); err != nil {
				return nil, nil, fmt.Errorf("file_pull: writing %s: %w", args.LocalPath, err)
			}
			return jsonResult(map[string]any{"vm_id": args.VMID, "local_path": args.LocalPath, "bytes": len(data)}), nil, nil
		}
		if utf8.Valid(data) {
			return jsonResult(map[string]any{"vm_id": args.VMID, "remote_path": args.RemotePath, "content": string(data)}), nil, nil
		}
		return jsonResult(map[string]any{
			"vm_id": args.VMID, "remote_path": args.RemotePath,
			"content_base64": base64.StdEncoding.EncodeToString(data),
		}), nil, nil
	})
}
