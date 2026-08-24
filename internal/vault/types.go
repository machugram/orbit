package vault

// CredentialInfo describes a stored secret without exposing its value.
// Returned by ListCredentials (the cred_list MCP tool lists keys, not values).
type CredentialInfo struct {
	ID        string
	Namespace string
	Key       string
	Label     string
	VMID      string
	CreatedAt int64 // unix seconds
	RotatedAt int64 // unix seconds, 0 if never rotated
}

// VMState is the lifecycle state of an agent-managed VM.
type VMState string

const (
	VMCreating  VMState = "creating"
	VMRunning   VMState = "running"
	VMStopped   VMState = "stopped"
	VMDestroyed VMState = "destroyed"
)

// VM is a record of an agent-managed OrbStack machine.
type VM struct {
	ID          string // matches the OrbStack machine name
	Distro      string
	State       VMState
	KeyID       string // credentials.id of the VM's SSH keypair, "" if none
	CreatedAt   int64  // unix seconds
	DestroyedAt int64  // unix seconds, 0 if not destroyed
	Ephemeral   bool   // auto-destroy when idle past TTL
	TTLSeconds  int64  // idle TTL when Ephemeral; 0 if not ephemeral
	LastUsedAt  int64  // unix seconds; 0 means fall back to CreatedAt
	Namespace   string // agent namespace (SharedNamespace for warm bases)
}

// MicroVM is a record of a nested QEMU guest running inside a carrier OrbStack
// machine. It has its own kernel (so eBPF loaded inside it cannot touch the
// shared OrbStack kernel) and is reached over SSH at <carrier>.orb.local:Port,
// a port QEMU forwards into the guest. See docs/adr/ADR-012.
type MicroVM struct {
	ID          string  // logical name, unique across microVMs
	Carrier     string  // OrbStack machine hosting the QEMU process
	Image       string  // image key (e.g. "ubuntu", "alpine")
	KeyID       string  // credentials.id of the guest's SSH keypair
	SSHPort     int     // host-forwarded port on the carrier reaching guest:22
	Handle      string  // on-carrier working directory / control handle
	State       VMState // reuses the VM lifecycle states
	CreatedAt   int64   // unix seconds
	DestroyedAt int64   // unix seconds, 0 if not destroyed
	Namespace   string  // agent namespace
}

// SetOption customizes a credential written via SetCredential.
type SetOption func(*setOptions)

type setOptions struct {
	label string
	vmID  string
}

// WithLabel attaches a human-readable label to the credential.
func WithLabel(label string) SetOption {
	return func(o *setOptions) { o.label = label }
}

// WithVM associates the credential with a VM record.
func WithVM(vmID string) SetOption {
	return func(o *setOptions) { o.vmID = vmID }
}
