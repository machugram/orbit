// Package exec runs commands inside agent-managed VMs over SSH using
// keypairs decrypted from the vault — private keys live in memory only and
// never touch disk (improving on spec §5's temp-file approach).
//
// Connections go directly to the in-VM sshd at <vm>.orb.local:22, not to
// OrbStack's SSH multiplexer (127.0.0.1:32222) as spec §5 assumed: the
// multiplexer authenticates only with OrbStack's own key and ignores in-VM
// authorized_keys, which would defeat the per-VM credential model. The
// provisioner installs and enables openssh-server for this reason.
package exec

import "time"

// Result is the outcome of a command executed on a VM.
type Result struct {
	VMID     string `json:"vm_id"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// ParallelResult pairs a VM with its result or error from ExecParallel.
type ParallelResult struct {
	VMID   string  `json:"vm_id"`
	Result *Result `json:"result,omitempty"`
	Err    string  `json:"error,omitempty"`
}

// DefaultDialTimeout bounds the TCP+handshake phase of each connection.
const DefaultDialTimeout = 15 * time.Second
