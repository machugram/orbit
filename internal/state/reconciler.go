package state

import (
	"context"
	"fmt"
	"strings"

	"github.com/machugram/orbit/internal/container"
	"github.com/machugram/orbit/internal/vm"
)

// Reconciler diffs a desired State against live Orbit resources and drives
// the VM and container managers to converge.
type Reconciler struct {
	vms        *vm.Manager
	prov       *vm.Provisioner
	containers *container.Manager
}

// NewReconciler returns a Reconciler wired to the given managers.
func NewReconciler(vms *vm.Manager, prov *vm.Provisioner, containers *container.Manager) *Reconciler {
	return &Reconciler{vms: vms, prov: prov, containers: containers}
}

// Plan computes what Apply would do without executing anything.
func (r *Reconciler) Plan(ctx context.Context, desired *State) (*Plan, error) {
	actions, err := r.diff(ctx, desired, false)
	if err != nil {
		return nil, err
	}
	return buildPlan(actions), nil
}

// PlanDestroy computes what Destroy would do without executing anything.
func (r *Reconciler) PlanDestroy(ctx context.Context, desired *State) (*Plan, error) {
	actions, err := r.diff(ctx, desired, true)
	if err != nil {
		return nil, err
	}
	return buildPlan(actions), nil
}

// Apply converges live state toward desired. It creates or runs missing
// resources; it never removes resources that are not listed in desired.
func (r *Reconciler) Apply(ctx context.Context, desired *State) (*ApplyResult, error) {
	actions, err := r.diff(ctx, desired, false)
	if err != nil {
		return nil, err
	}
	return r.execute(ctx, actions, desired)
}

// Destroy removes all resources named in desired.
func (r *Reconciler) Destroy(ctx context.Context, desired *State) (*ApplyResult, error) {
	actions, err := r.diff(ctx, desired, true)
	if err != nil {
		return nil, err
	}
	return r.execute(ctx, actions, desired)
}

// diff computes the action list. When destroyMode is true it produces
// destroy/remove/down actions for resources that exist; otherwise it produces
// create/run/up for resources that are missing.
func (r *Reconciler) diff(ctx context.Context, desired *State, destroyMode bool) ([]Action, error) {
	var actions []Action

	// --- VMs ---
	existingVMs, err := r.vms.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("state: list vms: %w", err)
	}
	vmByName := map[string]bool{}
	for _, v := range existingVMs {
		vmByName[v.ID] = true
	}

	for _, spec := range desired.VMs {
		exists := vmByName[spec.Name]
		if destroyMode {
			if exists {
				actions = append(actions, Action{Kind: ActionDestroy, Resource: "vm", Name: spec.Name})
			} else {
				actions = append(actions, Action{Kind: ActionNoop, Resource: "vm", Name: spec.Name, Detail: "not found"})
			}
		} else {
			if !exists {
				actions = append(actions, Action{Kind: ActionCreate, Resource: "vm", Name: spec.Name, Detail: spec.Distro})
			} else {
				actions = append(actions, Action{Kind: ActionNoop, Resource: "vm", Name: spec.Name, Detail: "already running"})
			}
		}
	}

	// --- Containers ---
	existingContainers, err := r.containers.List(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("state: list containers: %w", err)
	}
	containerByName := map[string]string{} // name -> state
	for _, c := range existingContainers {
		containerByName[c.Name] = c.State
	}

	for _, spec := range desired.Containers {
		name := spec.Name
		if name == "" {
			name = strings.ReplaceAll(spec.Image, "/", "-")
			name = strings.ReplaceAll(name, ":", "-")
		}
		state, exists := containerByName[name]
		if destroyMode {
			if exists {
				actions = append(actions, Action{Kind: ActionRemove, Resource: "container", Name: name, Detail: state})
			} else {
				actions = append(actions, Action{Kind: ActionNoop, Resource: "container", Name: name, Detail: "not found"})
			}
		} else {
			if !exists {
				actions = append(actions, Action{Kind: ActionRun, Resource: "container", Name: name, Detail: spec.Image})
			} else {
				actions = append(actions, Action{Kind: ActionNoop, Resource: "container", Name: name, Detail: "already " + state})
			}
		}
	}

	// --- Composes ---
	for _, spec := range desired.Composes {
		if destroyMode {
			actions = append(actions, Action{Kind: ActionDown, Resource: "compose", Name: spec.Project})
		} else {
			actions = append(actions, Action{Kind: ActionUp, Resource: "compose", Name: spec.Project})
		}
	}

	return actions, nil
}

func (r *Reconciler) execute(ctx context.Context, actions []Action, desired *State) (*ApplyResult, error) {
	res := &ApplyResult{}

	// index desired specs for quick lookup during execution
	vmSpecs := map[string]VMSpec{}
	for _, v := range desired.VMs {
		vmSpecs[v.Name] = v
	}
	containerSpecs := map[string]ContainerSpec{}
	for _, c := range desired.Containers {
		name := c.Name
		if name == "" {
			name = strings.ReplaceAll(c.Image, "/", "-")
			name = strings.ReplaceAll(name, ":", "-")
		}
		containerSpecs[name] = c
	}
	composeSpecs := map[string]ComposeSpec{}
	for _, cs := range desired.Composes {
		composeSpecs[cs.Project] = cs
	}

	for _, a := range actions {
		if a.Kind == ActionNoop {
			res.Applied = append(res.Applied, a)
			continue
		}

		var execErr error
		switch a.Kind {
		case ActionCreate:
			spec := vmSpecs[a.Name]
			isolated := spec.Isolated == nil || *spec.Isolated
			_, execErr = r.prov.Create(ctx, spec.Distro, spec.Name, isolated)

		case ActionDestroy:
			execErr = r.vms.Destroy(ctx, a.Name, true)

		case ActionRun:
			cs := containerSpecs[a.Name]
			_, execErr = r.containers.Run(ctx, container.RunSpec{
				Image:  cs.Image,
				Name:   cs.Name,
				Ports:  cs.Ports,
				Env:    cs.Env,
				Detach: true,
			})

		case ActionRemove:
			_ = r.containers.Stop(ctx, a.Name)
			execErr = r.containers.Remove(ctx, a.Name, true)

		case ActionUp:
			cs := composeSpecs[a.Name]
			_, execErr = r.containers.ComposeUp(ctx, container.ComposeSpec{
				Project: cs.Project,
				File:    cs.File,
				YAML:    cs.YAML,
			})

		case ActionDown:
			cs := composeSpecs[a.Name]
			execErr = r.containers.ComposeDown(ctx, container.ComposeSpec{
				Project: cs.Project,
				File:    cs.File,
				YAML:    cs.YAML,
			})
		}

		if execErr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s %s/%s: %v", a.Kind, a.Resource, a.Name, execErr))
		} else {
			res.Applied = append(res.Applied, a)
		}
	}

	noops := 0
	for _, a := range res.Applied {
		if a.Kind == ActionNoop {
			noops++
		}
	}
	real := len(res.Applied) - noops
	res.Summary = fmt.Sprintf("%d action(s) applied, %d already satisfied, %d error(s)", real, noops, len(res.Errors))
	return res, nil
}

func buildPlan(actions []Action) *Plan {
	noops := 0
	for _, a := range actions {
		if a.Kind == ActionNoop {
			noops++
		}
	}
	real := len(actions) - noops
	return &Plan{
		Actions: actions,
		Summary: fmt.Sprintf("%d change(s) pending, %d already satisfied", real, noops),
	}
}
