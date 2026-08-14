package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/vault"
)

type credSetArgs struct {
	Key       string `json:"key" jsonschema:"name of the secret"`
	Value     string `json:"value" jsonschema:"secret value (encrypted at rest)"`
	Namespace string `json:"namespace,omitempty" jsonschema:"vault namespace (default 'default')"`
	Label     string `json:"label,omitempty" jsonschema:"human-readable label"`
}

type credKeyArgs struct {
	Key       string `json:"key" jsonschema:"name of the secret"`
	Namespace string `json:"namespace,omitempty" jsonschema:"vault namespace (default 'default')"`
}

type credListArgs struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"vault namespace (default 'default')"`
}

func registerCredTools(server *mcp.Server, d *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cred_set",
		Description: "Store an arbitrary secret in the encrypted vault (AES-256-GCM at rest). Overwriting an existing key rotates it in place.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args credSetArgs) (*mcp.CallToolResult, any, error) {
		var opts []vault.SetOption
		if args.Label != "" {
			opts = append(opts, vault.WithLabel(args.Label))
		}
		id, err := d.Vault.SetCredential(args.Namespace, args.Key, []byte(args.Value), opts...)
		d.log("cred_set", map[string]any{"namespace": args.Namespace, "key": args.Key}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"id": id, "key": args.Key, "status": "stored"}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cred_get",
		Description: "Retrieve a secret from the vault. The value is returned to the caller (including the model); prefer secret_env on container/compose tools when the secret must stay out of the transcript.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args credKeyArgs) (*mcp.CallToolResult, any, error) {
		val, err := d.Vault.GetCredential(args.Namespace, args.Key)
		d.log("cred_get", map[string]any{"namespace": args.Namespace, "key": args.Key}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"key": args.Key, "value": string(val)}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cred_delete",
		Description: "Delete a secret from the vault.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args credKeyArgs) (*mcp.CallToolResult, any, error) {
		err := d.Vault.DeleteCredential(args.Namespace, args.Key)
		d.log("cred_delete", map[string]any{"namespace": args.Namespace, "key": args.Key}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"key": args.Key, "status": "deleted"}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cred_list",
		Description: "List secret names (never values) in a vault namespace.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args credListArgs) (*mcp.CallToolResult, any, error) {
		infos, err := d.Vault.ListCredentials(args.Namespace)
		d.log("cred_list", map[string]any{"namespace": args.Namespace}, err)
		if err != nil {
			return nil, nil, err
		}
		type credView struct {
			Key       string `json:"key"`
			Label     string `json:"label,omitempty"`
			VMID      string `json:"vm_id,omitempty"`
			CreatedAt int64  `json:"created_at"`
		}
		out := make([]credView, len(infos))
		for i, c := range infos {
			out[i] = credView{Key: c.Key, Label: c.Label, VMID: c.VMID, CreatedAt: c.CreatedAt}
		}
		return jsonResult(out), nil, nil
	})
}
