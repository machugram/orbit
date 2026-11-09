package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/exec"
)

type filePutArgs struct {
	Target        string `json:"target" jsonschema:"workspace name"`
	Path          string `json:"path" jsonschema:"path inside the workspace"`
	Content       string `json:"content,omitempty" jsonschema:"file text"`
	ContentBase64 string `json:"content_base64,omitempty" jsonschema:"file bytes, base64"`
}

type fileGetArgs struct {
	Target string `json:"target" jsonschema:"workspace name"`
	Path   string `json:"path" jsonschema:"path inside the workspace"`
}

func registerFile(server *mcp.Server, d *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "file_put",
		Description: "Write a file into a workspace. content for text, content_base64 otherwise. Parent directories are created. Capped at 32 MiB.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args filePutArgs) (*mcp.CallToolResult, any, error) {
		n, err := d.filePut(ctx, args)
		d.log("file_put", map[string]any{"target": args.Target, "path": args.Path, "bytes": n}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"target": args.Target, "path": args.Path, "bytes": n}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "file_get",
		Description: "Read a file from a workspace. Text as content, other bytes as content_base64. Capped at 32 MiB.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args fileGetArgs) (*mcp.CallToolResult, any, error) {
		out, err := d.fileGet(ctx, args)
		d.log("file_get", map[string]any{"target": args.Target, "path": args.Path}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(out), nil, nil
	})
}

func (d *Deps) filePut(ctx context.Context, args filePutArgs) (int, error) {
	if args.Content != "" && args.ContentBase64 != "" {
		return 0, fmt.Errorf("pass content or content_base64, not both")
	}
	data := []byte(args.Content)
	if args.ContentBase64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(args.ContentBase64)
		if err != nil {
			return 0, fmt.Errorf("content_base64: %w", err)
		}
		data = decoded
	}
	if len(data) > exec.MaxPullSize {
		return 0, fmt.Errorf("%s exceeds the %d byte limit", args.Path, exec.MaxPullSize)
	}
	kind, ok, err := d.findWorkspace(ctx, args.Target)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, missingWorkspace(args.Target)
	}
	if err := d.startWorkspace(ctx, args.Target, kind); err != nil {
		return 0, err
	}
	switch kind {
	case kindVM, kindMicroVM:
		err = d.Runner.PushBytes(ctx, args.Target, data, args.Path, 0o644)
	default:
		err = d.Containers.PutBytes(ctx, args.Target, args.Path, data)
	}
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func (d *Deps) fileGet(ctx context.Context, args fileGetArgs) (map[string]any, error) {
	kind, ok, err := d.findWorkspace(ctx, args.Target)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, missingWorkspace(args.Target)
	}
	if err := d.startWorkspace(ctx, args.Target, kind); err != nil {
		return nil, err
	}
	var data []byte
	switch kind {
	case kindVM, kindMicroVM:
		data, err = d.Runner.Pull(ctx, args.Target, args.Path)
	default:
		data, err = d.Containers.GetBytes(ctx, args.Target, args.Path)
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{"target": args.Target, "path": args.Path, "bytes": len(data)}
	if utf8.Valid(data) {
		out["content"] = string(data)
	} else {
		out["content_base64"] = base64.StdEncoding.EncodeToString(data)
	}
	return out, nil
}
