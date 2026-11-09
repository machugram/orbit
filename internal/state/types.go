// Package state provides declarative desired-state reconciliation over Orbit's
// imperative VM and container primitives. A caller describes what should exist;
// the Reconciler diffs that against live reality and issues the minimum set of
// create/destroy/run/remove calls to converge.
package state

import (
	"encoding/json"
	"fmt"
)

// State is the desired-state document. It is parsed from JSON supplied by the
// model or a file on disk.
type State struct {
	VMs        []VMSpec        `json:"vms,omitempty"`
	Containers []ContainerSpec `json:"containers,omitempty"`
	Composes   []ComposeSpec   `json:"composes,omitempty"`
}

// VMSpec describes one OrbStack Linux machine that should exist.
type VMSpec struct {
	Name     string `json:"name"`
	Distro   string `json:"distro"`
	Isolated *bool  `json:"isolated,omitempty"`
}

// ContainerSpec describes one Docker container that should be running.
type ContainerSpec struct {
	Image string            `json:"image"`
	Name  string            `json:"name,omitempty"`
	Ports []string          `json:"ports,omitempty"`
	Env   map[string]string `json:"env,omitempty"`
}

// ComposeSpec describes a docker-compose stack that should be up.
type ComposeSpec struct {
	Project string `json:"project"`
	File    string `json:"file,omitempty"`
	YAML    string `json:"yaml,omitempty"`
}

// ActionKind classifies a planned operation.
type ActionKind string

const (
	ActionCreate  ActionKind = "create"
	ActionDestroy ActionKind = "destroy"
	ActionRun     ActionKind = "run"
	ActionRemove  ActionKind = "remove"
	ActionUp      ActionKind = "up"
	ActionDown    ActionKind = "down"
	ActionNoop    ActionKind = "noop"
)

// Action is one step in a plan.
type Action struct {
	Kind     ActionKind `json:"kind"`
	Resource string     `json:"resource"` // "vm", "container", "compose"
	Name     string     `json:"name"`
	Detail   string     `json:"detail,omitempty"`
}

// Plan is the output of a dry-run: what would Apply do.
type Plan struct {
	Actions []Action `json:"actions"`
	Summary string   `json:"summary"`
}

// ApplyResult records what Apply actually did.
type ApplyResult struct {
	Applied []Action `json:"applied"`
	Errors  []string `json:"errors,omitempty"`
	Summary string   `json:"summary"`
}

// Validate checks a State supplied either by ParseState or by typed MCP tool
// arguments before reconciliation runs.
func (s *State) Validate() error {
	seen := map[string]bool{}
	for _, v := range s.VMs {
		if v.Name == "" {
			return fmt.Errorf("state: vm entry missing name")
		}
		if v.Distro == "" {
			return fmt.Errorf("state: vm %q missing distro", v.Name)
		}
		if seen["vm:"+v.Name] {
			return fmt.Errorf("state: duplicate vm name %q", v.Name)
		}
		seen["vm:"+v.Name] = true
	}
	for _, c := range s.Containers {
		if c.Image == "" {
			return fmt.Errorf("state: container entry missing image")
		}
		key := "container:" + c.Name
		if c.Name == "" {
			key = "container:" + c.Image
		}
		if seen[key] {
			return fmt.Errorf("state: duplicate container name/image %q", c.Name)
		}
		seen[key] = true
	}
	for _, cs := range s.Composes {
		if cs.Project == "" {
			return fmt.Errorf("state: compose entry missing project")
		}
		if cs.File == "" && cs.YAML == "" {
			return fmt.Errorf("state: compose %q needs file or yaml", cs.Project)
		}
		if seen["compose:"+cs.Project] {
			return fmt.Errorf("state: duplicate compose project %q", cs.Project)
		}
		seen["compose:"+cs.Project] = true
	}
	return nil
}

// ParseState decodes a JSON state document.
func ParseState(data []byte) (*State, error) {
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("state: invalid JSON: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}
