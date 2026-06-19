package exec

import (
	"context"
	"sync"
)

// DefaultParallelism bounds concurrent SSH sessions in ExecParallel.
const DefaultParallelism = 8

// ExecParallel runs the same command on every VM concurrently and returns
// one entry per VM in input order. Individual VM failures are reported in
// their entry and do not cancel the others.
func (r *Runner) ExecParallel(ctx context.Context, vmIDs []string, command string, limit int) []ParallelResult {
	if limit <= 0 {
		limit = DefaultParallelism
	}
	results := make([]ParallelResult, len(vmIDs))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for i, id := range vmIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res, err := r.Exec(ctx, id, command)
			pr := ParallelResult{VMID: id, Result: res}
			if err != nil {
				pr.Err = err.Error()
			}
			results[i] = pr
		}()
	}
	wg.Wait()
	return results
}
