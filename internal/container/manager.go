package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"sort"
	"strconv"
	"strings"

	"orbit/internal/vault"
)

// Manager runs Docker and Compose commands on OrbStack's engine, resolving
// vault secrets into container environments.
type Manager struct {
	store *vault.Store
	run   DockerRunner
}

// Option customizes a Manager.
type Option func(*Manager)

// WithRunner replaces the docker subprocess runner (used by tests).
func WithRunner(r DockerRunner) Option {
	return func(m *Manager) { m.run = r }
}

// NewManager returns a Manager backed by the vault and the real docker CLI
// unless overridden with WithRunner.
func NewManager(store *vault.Store, opts ...Option) *Manager {
	m := &Manager{store: store, run: dockerCLI}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// dockerCLI is the default DockerRunner: a real docker subprocess. Resolved
// secret values are placed in the subprocess environment (never argv).
func dockerCLI(ctx context.Context, inv Invocation) ([]byte, []byte, error) {
	cmd := osexec.CommandContext(ctx, "docker", inv.Args...)
	if len(inv.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(inv.Stdin)
	}
	if len(inv.Env) > 0 {
		env := os.Environ()
		for k, v := range inv.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		// Surface a clean error, but never echo inv.Env (it holds secrets).
		err = fmt.Errorf("container: docker %s: %w: %s",
			strings.Join(inv.Args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// resolveSecrets reads each vault secret named in secretEnv and returns the
// subprocess env map (var name -> value) plus the bare `-e NAME` flags to
// hand docker. Values are returned only in the env map, never logged.
func (m *Manager) resolveSecrets(namespace string, secretEnv map[string]string) (env map[string]string, flags []string, err error) {
	if len(secretEnv) == 0 {
		return nil, nil, nil
	}
	env = make(map[string]string, len(secretEnv))
	// Deterministic flag order keeps invocations testable.
	for _, name := range sortedKeys(secretEnv) {
		val, err := m.store.GetCredential(namespace, secretEnv[name])
		if err != nil {
			return nil, nil, fmt.Errorf("container: resolving secret %q for env %q: %w", secretEnv[name], name, err)
		}
		env[name] = string(val)
		flags = append(flags, "-e", name)
	}
	return env, flags, nil
}

// bareEnvFlags returns deterministic `-e NAME` flags (no value) for every key
// in env. The values travel through the docker subprocess environment instead
// of argv, so neither secrets nor literal env vars appear in `ps`/`/proc`.
func bareEnvFlags(env map[string]string) []string {
	flags := make([]string, 0, len(env)*2)
	for _, name := range sortedKeys(env) {
		flags = append(flags, "-e", name)
	}
	return flags
}

// sortedKeys returns a map's keys in deterministic order, so generated docker
// invocations are stable and testable.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Run starts a container and returns its ID (detached) or its output.
func (m *Manager) Run(ctx context.Context, spec RunSpec) (string, error) {
	if err := spec.validate(); err != nil {
		return "", err
	}
	secretEnv, _, err := m.resolveSecrets(spec.Namespace, spec.SecretEnv)
	if err != nil {
		return "", err
	}
	// Merge literal env and resolved secrets into one subprocess env map;
	// secrets win on a name clash. Only bare `-e NAME` flags reach argv, so no
	// value (literal or secret) is ever visible in `ps`/`/proc`.
	env := make(map[string]string, len(spec.Env)+len(secretEnv))
	for k, v := range spec.Env {
		env[k] = v
	}
	for k, v := range secretEnv {
		env[k] = v
	}

	args := []string{"run"}
	if spec.Detach {
		args = append(args, "-d")
	}
	if spec.Remove && !spec.Detach {
		args = append(args, "--rm")
	}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	for _, p := range spec.Ports {
		args = append(args, "-p", p)
	}
	args = append(args, bareEnvFlags(env)...)
	args = append(args, spec.Image)
	if spec.Command != "" {
		args = append(args, "sh", "-c", spec.Command)
	}

	stdout, _, err := m.run(ctx, Invocation{Args: args, Env: env})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(stdout)), nil
}

// Exec runs a shell command inside a running container.
func (m *Manager) Exec(ctx context.Context, name, command string) (*Result, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("container: invalid name %q", name)
	}
	args := []string{"exec", name, "sh", "-c", command}
	stdout, stderr, err := m.run(ctx, Invocation{Args: args})
	res := &Result{Stdout: string(stdout), Stderr: string(stderr)}
	if code, ok := exitCode(err); ok {
		res.ExitCode = code
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Logs returns recent logs for a container (tail<=0 means all).
func (m *Manager) Logs(ctx context.Context, name string, tail int) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("container: invalid name %q", name)
	}
	args := []string{"logs"}
	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	args = append(args, name)
	stdout, stderr, err := m.run(ctx, Invocation{Args: args})
	if err != nil {
		return "", err
	}
	// docker writes container stderr to our stderr; include both.
	return string(stdout) + string(stderr), nil
}

// List returns containers (all=true includes stopped ones).
func (m *Manager) List(ctx context.Context, all bool) ([]Container, error) {
	args := []string{"ps", "--format", "{{json .}}"}
	if all {
		args = append(args, "-a")
	}
	stdout, _, err := m.run(ctx, Invocation{Args: args})
	if err != nil {
		return nil, err
	}
	var out []Container
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		if line == "" {
			continue
		}
		var row struct {
			ID, Names, Image, State, Status, Ports string
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		out = append(out, Container{
			ID: row.ID, Name: row.Names, Image: row.Image,
			State: row.State, Status: row.Status, Ports: row.Ports,
		})
	}
	return out, nil
}

// Stop stops a running container.
func (m *Manager) Stop(ctx context.Context, name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("container: invalid name %q", name)
	}
	_, _, err := m.run(ctx, Invocation{Args: []string{"stop", name}})
	return err
}

// Remove deletes a container (force stops it first when force is set).
func (m *Manager) Remove(ctx context.Context, name string, force bool) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("container: invalid name %q", name)
	}
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, name)
	_, _, err := m.run(ctx, Invocation{Args: args})
	return err
}

// composeArgs builds the leading `compose -p <project> -f <src>` args and any
// stdin payload for inline YAML.
func (s *ComposeSpec) composeArgs() (args []string, stdin []byte) {
	args = []string{"compose", "-p", s.Project}
	if s.YAML != "" {
		args = append(args, "-f", "-")
		stdin = []byte(s.YAML)
	} else {
		args = append(args, "-f", s.File)
	}
	return args, stdin
}

// ComposeUp brings a compose project up in detached mode.
func (m *Manager) ComposeUp(ctx context.Context, spec ComposeSpec) (string, error) {
	if err := spec.validate(); err != nil {
		return "", err
	}
	secretEnv, _, err := m.resolveSecretsForCompose(spec)
	if err != nil {
		return "", err
	}
	base, stdin := spec.composeArgs()
	args := append(base, "up", "-d")
	// Deterministic --scale order keeps invocations testable.
	for _, svc := range sortedKeys(spec.Scale) {
		args = append(args, "--scale", svc+"="+strconv.Itoa(spec.Scale[svc]))
	}
	stdout, stderr, err := m.run(ctx, Invocation{Args: args, Stdin: stdin, Env: secretEnv})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(stdout) + string(stderr)), nil
}

// ComposeDown tears a compose project down.
func (m *Manager) ComposeDown(ctx context.Context, spec ComposeSpec) error {
	if err := spec.validate(); err != nil {
		return err
	}
	base, stdin := spec.composeArgs()
	args := append(base, "down")
	_, _, err := m.run(ctx, Invocation{Args: args, Stdin: stdin})
	return err
}

// ComposeLogs returns recent logs for a compose project.
func (m *Manager) ComposeLogs(ctx context.Context, spec ComposeSpec, tail int) (string, error) {
	if err := spec.validate(); err != nil {
		return "", err
	}
	base, stdin := spec.composeArgs()
	args := append(base, "logs", "--no-color")
	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	stdout, stderr, err := m.run(ctx, Invocation{Args: args, Stdin: stdin})
	if err != nil {
		return "", err
	}
	return string(stdout) + string(stderr), nil
}

// ComposePs lists a project's services with their state and the OrbStack
// hostname the host can reach each on. Stopped/crashed services are included
// so the agent can debug a partially-failed stack.
func (m *Manager) ComposePs(ctx context.Context, spec ComposeSpec) ([]ComposeService, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	base, stdin := spec.composeArgs()
	args := append(base, "ps", "-a", "--format", "json")
	stdout, _, err := m.run(ctx, Invocation{Args: args, Stdin: stdin})
	if err != nil {
		return nil, err
	}
	return parseComposePs(stdout), nil
}

// ComposeExec runs a shell command inside a project's service. It resolves the
// service's running container via ComposePs and reuses the plain `docker exec`
// path — avoiding the conflict between an inline compose file on stdin and the
// command's own stdin.
func (m *Manager) ComposeExec(ctx context.Context, spec ComposeSpec, service, command string) (*Result, error) {
	if !serviceRe.MatchString(service) {
		return nil, fmt.Errorf("container: invalid service name %q", service)
	}
	services, err := m.ComposePs(ctx, spec)
	if err != nil {
		return nil, err
	}
	for _, s := range services {
		if s.Service == service && isRunning(s.State) {
			return m.Exec(ctx, s.Name, command)
		}
	}
	return nil, fmt.Errorf("container: no running container for service %q in project %q", service, spec.Project)
}

// ComposeBuild builds images for a project (all services, or the named ones).
func (m *Manager) ComposeBuild(ctx context.Context, spec ComposeSpec, services []string) (string, error) {
	if err := spec.validate(); err != nil {
		return "", err
	}
	for _, svc := range services {
		if !serviceRe.MatchString(svc) {
			return "", fmt.Errorf("container: invalid service name %q", svc)
		}
	}
	base, stdin := spec.composeArgs()
	args := append(base, "build")
	args = append(args, services...)
	stdout, stderr, err := m.run(ctx, Invocation{Args: args, Stdin: stdin})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(stdout) + string(stderr)), nil
}

// isRunning reports whether a compose ps State means the container is up.
func isRunning(state string) bool {
	return state == "running" || strings.HasPrefix(state, "Up")
}

// parseComposePs handles both the JSON-array and the newer JSON-lines output
// of `docker compose ps --format json`.
func parseComposePs(out []byte) []ComposeService {
	type publisher struct {
		PublishedPort int
		TargetPort    int
		Protocol      string
	}
	type rawRow struct {
		Name, Service, Image, State, Health string
		Publishers                          []publisher
	}
	trimmed := bytes.TrimSpace(out)
	var rows []rawRow
	if len(trimmed) > 0 && trimmed[0] == '[' {
		_ = json.Unmarshal(trimmed, &rows)
	} else {
		for _, line := range strings.Split(string(trimmed), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var r rawRow
			if json.Unmarshal([]byte(line), &r) == nil {
				rows = append(rows, r)
			}
		}
	}
	out2 := make([]ComposeService, 0, len(rows))
	for _, r := range rows {
		var ports []string
		for _, p := range r.Publishers {
			if p.PublishedPort > 0 {
				ports = append(ports, fmt.Sprintf("%d->%d/%s", p.PublishedPort, p.TargetPort, p.Protocol))
			}
		}
		svc := ComposeService{
			Service: r.Service, Name: r.Name, State: r.State,
			Health: r.Health, Image: r.Image, Ports: strings.Join(ports, ", "),
		}
		if r.Name != "" {
			svc.OrbHost = r.Name + ".orb.local"
		}
		out2 = append(out2, svc)
	}
	return out2
}

// resolveSecretsForCompose injects compose ${VAR} values from the vault. For
// inline YAML the env is consumed at parse time; for file-based projects
// compose reads the same process environment.
func (m *Manager) resolveSecretsForCompose(spec ComposeSpec) (map[string]string, []string, error) {
	env, flags, err := m.resolveSecrets(spec.Namespace, spec.SecretEnv)
	if err != nil {
		return nil, nil, err
	}
	if len(spec.Env) > 0 {
		if env == nil {
			env = make(map[string]string, len(spec.Env))
		}
		for k, v := range spec.Env {
			env[k] = v
		}
	}
	return env, flags, nil
}

// Build builds an image, optionally from an inline Dockerfile.
func (m *Manager) Build(ctx context.Context, spec BuildSpec) (string, error) {
	if err := spec.validate(); err != nil {
		return "", err
	}
	args := []string{"build", "-t", spec.Tag}
	for _, k := range sortedKeys(spec.BuildArgs) {
		args = append(args, "--build-arg", k+"="+spec.BuildArgs[k])
	}
	var stdin []byte
	if spec.Dockerfile != "" {
		args = append(args, "-f", "-")
		stdin = []byte(spec.Dockerfile)
	}
	args = append(args, spec.ContextPath)
	stdout, stderr, err := m.run(ctx, Invocation{Args: args, Stdin: stdin})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(stdout) + string(stderr)), nil
}

// exitCode extracts a non-zero process exit code from a runner error so a
// failing command-in-container is reported as a result, not a tool error.
func exitCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var ee *osexec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}
