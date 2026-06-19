package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"orbit/internal/keys"
	"orbit/internal/vm"
)

type keyGenerateArgs struct {
	Label       string `json:"label" jsonschema:"unique label for the keypair (e.g. vm-sandbox-1)"`
	AssociateVM string `json:"associate_vm,omitempty" jsonschema:"VM name to associate the key with"`
}

type keyLabelArgs struct {
	Label string `json:"label" jsonschema:"label of the stored keypair"`
}

type keyInfo struct {
	ID        string `json:"key_id"`
	Label     string `json:"label"`
	VMID      string `json:"vm_id,omitempty"`
	CreatedAt int64  `json:"created_at"`
	RotatedAt int64  `json:"rotated_at,omitempty"`
}

func registerKeyTools(server *mcp.Server, d *Deps) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "key_generate",
		Description: "Generate a new Ed25519 SSH keypair. The private key is encrypted into the vault; only the public key is returned.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args keyGenerateArgs) (*mcp.CallToolResult, any, error) {
		id, kp, err := d.Keys.Generate(args.Label, args.AssociateVM)
		d.log("key_generate", map[string]any{"label": args.Label, "associate_vm": args.AssociateVM}, err)
		if err != nil {
			return nil, nil, err
		}
		if args.AssociateVM != "" {
			// Best-effort: the VM record may not exist yet.
			d.VMs.SetKey(args.AssociateVM, id)
		}
		pub, err := kp.AuthorizedKey()
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"key_id": id, "label": args.Label, "public_key": pub}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "key_list",
		Description: "List all SSH keypairs in the vault (metadata only, never key material).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		infos, err := d.Keys.List()
		d.log("key_list", nil, err)
		if err != nil {
			return nil, nil, err
		}
		out := make([]keyInfo, len(infos))
		for i, c := range infos {
			out[i] = keyInfo{ID: c.ID, Label: c.Key, VMID: c.VMID, CreatedAt: c.CreatedAt, RotatedAt: c.RotatedAt}
		}
		return jsonResult(out), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "key_export_public",
		Description: "Return the public half of a stored keypair as an authorized_keys line (safe to share).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args keyLabelArgs) (*mcp.CallToolResult, any, error) {
		pub, err := d.Keys.AuthorizedKey(args.Label)
		d.log("key_export_public", map[string]any{"label": args.Label}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"label": args.Label, "public_key": pub}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "key_revoke",
		Description: "Delete a keypair from the vault. Does not modify any VM's authorized_keys; rotate or destroy the VM for that.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args keyLabelArgs) (*mcp.CallToolResult, any, error) {
		err := d.Keys.Revoke(args.Label)
		d.log("key_revoke", map[string]any{"label": args.Label}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"label": args.Label, "status": "revoked"}), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "key_rotate",
		Description: "Generate a new keypair for a VM, replace the VM's authorized_keys with it over SSH, then store it in the vault under the same label (revoking the old key).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args vmIDArgs) (*mcp.CallToolResult, any, error) {
		pub, err := rotateKey(ctx, d, args.VMID)
		d.log("key_rotate", map[string]any{"vm_id": args.VMID}, err)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"vm_id": args.VMID, "public_key": pub, "status": "rotated"}), nil, nil
	})
}

// rotateKey implements the spec's key_rotate: new key in memory → push to
// the VM's authorized_keys (authenticated with the old key) → persist the
// new key in the vault under the same label.
func rotateKey(ctx context.Context, d *Deps, vmID string) (string, error) {
	rec, err := d.Vault.GetVM(vmID)
	if err != nil {
		return "", err
	}
	if rec.KeyID == "" {
		return "", fmt.Errorf("key_rotate: vm %s has no key to rotate", vmID)
	}

	label := vm.KeyLabel(vmID)
	newKP, err := keys.Generate(label)
	if err != nil {
		return "", err
	}
	pub, err := newKP.AuthorizedKey()
	if err != nil {
		return "", err
	}

	// Replace (not append): the old key stops working in the same step.
	cmd := fmt.Sprintf(`mkdir -p ~/.ssh && chmod 700 ~/.ssh && printf '%%s\n' '%s' > ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys`, pub)
	res, err := d.Runner.Exec(ctx, vmID, cmd)
	if err != nil {
		return "", fmt.Errorf("key_rotate: updating authorized_keys on %s: %w", vmID, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("key_rotate: updating authorized_keys on %s failed (exit %d): %s", vmID, res.ExitCode, res.Stderr)
	}

	keyID, err := d.Keys.Put(label, vmID, newKP)
	if err != nil {
		return "", err
	}
	if err := d.VMs.SetKey(vmID, keyID); err != nil {
		return "", err
	}
	return pub, nil
}
