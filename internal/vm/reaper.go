package vm

import (
	"context"
	"log"
	"time"

	"github.com/machugram/orbit/internal/exec"
	"github.com/machugram/orbit/internal/vault"
)

// DefaultReaperInterval is how often the idle-TTL reaper scans for expired VMs.
const DefaultReaperInterval = 30 * time.Second

// Reaper destroys ephemeral VMs that have been idle longer than their TTL.
// Warm bases (orbit-base-*) are never targeted.
type Reaper struct {
	vms      *Manager
	runner   *exec.Runner
	now      func() time.Time
	interval time.Duration
}

// NewReaper returns a reaper wired to the VM manager and optional SSH pool.
func NewReaper(vms *Manager, runner *exec.Runner) *Reaper {
	return &Reaper{
		vms:      vms,
		runner:   runner,
		now:      time.Now,
		interval: DefaultReaperInterval,
	}
}

// WithNow injects a clock (tests).
func (r *Reaper) WithNow(now func() time.Time) *Reaper {
	r.now = now
	return r
}

// WithInterval overrides the scan period.
func (r *Reaper) WithInterval(d time.Duration) *Reaper {
	r.interval = d
	return r
}

// Start runs RunOnce on a ticker until ctx is cancelled.
func (r *Reaper) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(r.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := r.RunOnce(ctx); err != nil {
					log.Printf("orbit: ephemeral reaper: %v", err)
				}
			}
		}
	}()
}

// RunOnce destroys every expired ephemeral VM (purge keys + invalidate SSH).
func (r *Reaper) RunOnce(ctx context.Context) error {
	vms, err := r.vms.store.ListVMs()
	if err != nil {
		return err
	}
	now := r.now().Unix()
	for i := range vms {
		vm := &vms[i]
		if !vm.Ephemeral || vm.TTLSeconds <= 0 {
			continue
		}
		if vm.Namespace == vault.SharedNamespace || IsBaseName(vm.ID) {
			continue
		}
		if vm.State == vault.VMDestroyed {
			continue
		}
		anchor := vm.LastUsedAt
		if anchor == 0 {
			anchor = vm.CreatedAt
		}
		if now < anchor+vm.TTLSeconds {
			continue
		}
		if err := r.vms.Destroy(ctx, vm.ID, true); err != nil {
			log.Printf("orbit: reaper destroy %s: %v", vm.ID, err)
			continue
		}
		if r.runner != nil {
			r.runner.Invalidate(vm.ID)
		}
	}
	return nil
}
