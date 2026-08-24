// Command orbit is the Orbit MCP server (ORBIT_SPEC.md): it
// lets an MCP client manage SSH keys in an encrypted vault, provision
// isolated OrbStack VMs with those keys, and execute work inside them.
//
// Configuration (environment):
//
//	ORBIT_VAULT_PATH        vault location (default ~/.orbit/vault.db)
//	ORBIT_AUDIT_PATH        audit log     (default ~/.orbit/audit.log)
//	ORBIT_PASSPHRASE        vault passphrase; if unset, the macOS Keychain
//	                        is used (auto-generated on first run)
//	ORBIT_AGENT_NAMESPACE   soft isolation for VMs/microVMs/creds default
//	                        namespace on one shared vault (default "default")
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/machugram/orbit/internal/audit"
	"github.com/machugram/orbit/internal/container"
	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/keychain"
	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/microvm"
	"github.com/machugram/orbit/internal/tools"
	"github.com/machugram/orbit/internal/vault"
	"github.com/machugram/orbit/internal/vm"
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
	agentNS := envOr("ORBIT_AGENT_NAMESPACE", vault.DefaultAgentNamespace)
	store, err := vault.Open(vaultPath, pass, vault.WithAgentNamespace(agentNS))
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
	// One Runner drives both plain VMs and nested microVMs: the microvm
	// resolver handles microVM ids and chains to the vault resolver for the
	// rest, so exec/exec_script/file_* work against either by name.
	resolver := microvm.NewResolver(store, km, exec.VaultResolver(store, km))
	runner := exec.NewRunnerWithResolver(resolver, exec.WithActivityTracker(exec.VaultActivity{Store: store}))
	deps := &tools.Deps{
		Vault:      store,
		Keys:       km,
		VMs:        vmm,
		Prov:       vm.NewProvisioner(km, vmm),
		Runner:     runner,
		Containers: container.NewManager(store),
		MicroVMs:   microvm.NewManager(store, km),
		Audit:      auditLog,
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "orbit", Version: version}, nil)
	tools.Register(server, deps)

	// Ephemeral idle-TTL reaper (ROADMAP 2.1).
	reaperCtx, reaperCancel := context.WithCancel(context.Background())
	defer reaperCancel()
	vm.NewReaper(vmm, runner).Start(reaperCtx)

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
