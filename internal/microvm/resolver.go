package microvm

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/vault"
)

// resolver maps a microVM id to its SSH endpoint on the carrier and its
// vault-held guest key. Unknown ids fall through to next (the vault VM
// resolver), so a single Runner drives both VMs and microVMs.
type resolver struct {
	store *vault.Store
	keys  *keys.Manager
	next  exec.Resolver
}

// NewResolver returns an exec.Resolver that understands microVM ids and
// delegates everything else to next.
func NewResolver(store *vault.Store, km *keys.Manager, next exec.Resolver) exec.Resolver {
	return &resolver{store: store, keys: km, next: next}
}

func (r *resolver) Resolve(ctx context.Context, id string) (exec.ConnInfo, error) {
	mv, err := r.store.GetMicroVM(id)
	if errors.Is(err, vault.ErrNotFound) {
		if r.next != nil {
			return r.next.Resolve(ctx, id)
		}
		return exec.ConnInfo{}, err
	}
	if err != nil {
		return exec.ConnInfo{}, err
	}
	if mv.State == vault.VMDestroyed {
		return exec.ConnInfo{}, fmt.Errorf("microvm: %s is destroyed", id)
	}
	if mv.KeyID == "" {
		return exec.ConnInfo{}, fmt.Errorf("microvm: %s has no associated key", id)
	}
	signer, err := r.keys.SignerByID(mv.KeyID)
	if err != nil {
		return exec.ConnInfo{}, err
	}
	return exec.ConnInfo{
		Addr:   mv.Carrier + ".orb.local:" + strconv.Itoa(mv.SSHPort),
		User:   "agent",
		Signer: signer,
	}, nil
}
