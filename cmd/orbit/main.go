// Command orbit is the Orbit MCP server (ORBIT_SPEC.md): it
// lets an MCP client manage SSH keys in an encrypted vault, provision
// isolated OrbStack VMs with those keys, and execute work inside them.
//
// Configuration (environment):
//
//	ORBIT_VAULT_PATH   vault location (default ~/.orbit/vault.db)
//	ORBIT_AUDIT_PATH   audit log     (default ~/.orbit/audit.log)
//	ORBIT_PASSPHRASE   vault passphrase; if unset, the macOS Keychain
//	                        is used (auto-generated on first run)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"orbit/internal/audit"
	"orbit/internal/container"
	"orbit/internal/exec"
	"orbit/internal/keychain"
	"orbit/internal/keys"
	"orbit/internal/tools"
	"orbit/internal/vault"
	"orbit/internal/vm"
)

const version = "0.1.0"

func main() {
	flag.Bool("version", false, "print version and exit")
	flag.Parse()

	// Handle --version flag
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			fmt.Println("orbit " + version)
			os.Exit(0)
		}
	}

	if err := run(); err != nil {
		log.Fatalf("orbit: %v", err)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	vaultPath := envOr("ORBIT_VAULT_PATH", filepath.Join(home, ".orbit", "vault.db"))
	auditPath := envOr("ORBIT_AUDIT_PATH", filepath.Join(home, ".orbit", "audit.log"))

	pass, err := keychain.Passphrase()
	if err != nil {
		return err
	}
	store, err := vault.Open(vaultPath, pass)
	if err != nil {
		return err
	}
	defer store.Close()

	auditLog, err := audit.Open(auditPath)
	if err != nil {
		return fmt.Errorf("opening audit log: %w", err)
	}
	defer auditLog.Close()

	km := keys.NewManager(store)
	vmm := vm.NewManager(store)
	deps := &tools.Deps{
		Vault:      store,
		Keys:       km,
		VMs:        vmm,
		Prov:       vm.NewProvisioner(km, vmm),
		Runner:     exec.NewRunner(store, km),
		Containers: container.NewManager(store),
		Audit:      auditLog,
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "orbit", Version: version}, nil)
	tools.Register(server, deps)

	// stdio transport: stdout is the protocol channel; logs go to stderr.
	log.SetOutput(os.Stderr)
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
