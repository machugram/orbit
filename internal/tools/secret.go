package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type secretPutArgs struct {
	Key   string `json:"key" jsonschema:"name later passed in secret_env"`
	Value string `json:"value" jsonschema:"stored encrypted; not returned"`
}

func registerSecret(server *mcp.Server, d *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "secret_put",
		Description: "Store a secret under a name for secret_env. The value is not returned. Names are listed at orbit://secrets.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args secretPutArgs) (*mcp.CallToolResult, any, error) {
		if args.Key == "" {
			return nil, nil, fmt.Errorf("secret_put needs a key")
		}
		_, err := d.Vault.SetCredential("", args.Key, []byte(args.Value))
		d.log("secret_put", map[string]any{"key": args.Key}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"key": args.Key, "status": "stored"}), nil, nil
	})
}
