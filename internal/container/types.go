// Package container manages Docker containers and Compose projects on
// OrbStack's built-in Docker engine. Its distinguishing feature over raw
// `docker` is vault secret injection: a secret named in the vault is read by
// Orbit at run time and handed to the container as an environment variable
// without its value ever entering the model's context, the command line, or
// the audit log.
package container

import (
	"context"
	"fmt"
	"regexp"
)

// DockerRunner executes a docker invocation. Injectable so tests run against
// a fake instead of the real `docker` CLI.
type DockerRunner func(ctx context.Context, inv Invocation) (stdout, stderr []byte, err error)

// Invocation is one docker subprocess call. Env carries resolved secret
// values for the subprocess environment only — they are passed to docker via
// bare `-e NAME` flags so the values never appear in argv (and thus never in
// `ps` output or the audit log).
type Invocation struct {
	Args  []string
	Stdin []byte
	Env   map[string]string
}

// Result is the outcome of a container command.
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Container is one row of `docker ps`.
type Container struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status"`
	Ports  string `json:"ports,omitempty"`
}

// RunSpec describes a container to start.
type RunSpec struct {
	Image     string            // required, e.g. "alpine" or "nginx:1.27"
	Name      string            // optional; docker generates one if empty
	Command   string            // optional; run via `sh -c` if set
	Env       map[string]string // literal env vars (NAME -> value)
	SecretEnv map[string]string // env var NAME -> vault key (value injected, never logged)
	Namespace string            // vault namespace for SecretEnv (default "default")
	Ports     []string          // "8080:80", "127.0.0.1:5432:5432"
	Detach    bool              // -d
	Remove    bool              // --rm (ignored when Detach)
}

// ComposeSpec describes a docker-compose invocation. Exactly one of File or
// YAML identifies the project definition.
type ComposeSpec struct {
	Project   string            // -p; required
	File      string            // path to a compose file on the host
	YAML      string            // inline compose YAML, piped via `-f -`
	Env       map[string]string // literal env for ${VAR} interpolation
	SecretEnv map[string]string // env var NAME -> vault key
	Namespace string            // vault namespace for SecretEnv
	Scale     map[string]int    // service -> replica count (compose up only)
}

// ComposeService is one service's runtime status within a project, including
// the OrbStack hostname the host can reach it on.
type ComposeService struct {
	Service string `json:"service"`
	Name    string `json:"name"` // container name
	State   string `json:"state"`
	Health  string `json:"health,omitempty"`
	Image   string `json:"image,omitempty"`
	Ports   string `json:"ports,omitempty"`    // published port summary
	OrbHost string `json:"orb_host,omitempty"` // <name>.orb.local
}

// BuildSpec describes an image build.
type BuildSpec struct {
	Tag         string            // -t; required
	ContextPath string            // build context dir on the host; required
	Dockerfile  string            // inline Dockerfile, piped via `-f -`; optional
	BuildArgs   map[string]string // --build-arg
}

// MaxTransfer caps file copies into and out of a container.
const MaxTransfer = 32 << 20 // 32 MiB

var (
	nameRe        = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	imageRe       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./:@-]{0,255}$`)
	envRe         = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	portRe        = regexp.MustCompile(`^(\d{1,3}(\.\d{1,3}){3}:)?(\d{1,5}:)?\d{1,5}(/(tcp|udp))?$`)
	tagRe         = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./:-]{0,255}$`)
	projectRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	serviceRe     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	interpreterRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]*$`)
)

func (s *RunSpec) validate() error {
	if !imageRe.MatchString(s.Image) {
		return fmt.Errorf("container: invalid image %q", s.Image)
	}
	if s.Name != "" && !nameRe.MatchString(s.Name) {
		return fmt.Errorf("container: invalid name %q", s.Name)
	}
	if err := validateEnvNames(s.Env); err != nil {
		return err
	}
	if err := validateEnvNames(s.SecretEnv); err != nil {
		return err
	}
	for _, p := range s.Ports {
		if !portRe.MatchString(p) {
			return fmt.Errorf("container: invalid port mapping %q", p)
		}
	}
	return nil
}

func (s *ComposeSpec) validate() error {
	if !projectRe.MatchString(s.Project) {
		return fmt.Errorf("container: invalid compose project %q", s.Project)
	}
	if s.File == "" && s.YAML == "" {
		return fmt.Errorf("container: compose needs a file path or inline yaml")
	}
	if s.File != "" && s.YAML != "" {
		return fmt.Errorf("container: compose file and inline yaml are mutually exclusive")
	}
	if err := validateEnvNames(s.Env); err != nil {
		return err
	}
	for svc, n := range s.Scale {
		if !serviceRe.MatchString(svc) {
			return fmt.Errorf("container: invalid service name %q", svc)
		}
		if n < 0 {
			return fmt.Errorf("container: invalid scale %d for %q", n, svc)
		}
	}
	return validateEnvNames(s.SecretEnv)
}

func (s *BuildSpec) validate() error {
	if !tagRe.MatchString(s.Tag) {
		return fmt.Errorf("container: invalid image tag %q", s.Tag)
	}
	if s.ContextPath == "" {
		return fmt.Errorf("container: build needs a context path")
	}
	return validateEnvNames(s.BuildArgs)
}

func validateEnvNames(m map[string]string) error {
	for name := range m {
		if !envRe.MatchString(name) {
			return fmt.Errorf("container: invalid env var name %q", name)
		}
	}
	return nil
}
