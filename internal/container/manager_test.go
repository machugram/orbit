package container

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/machugram/orbit/internal/vault"
)

// recorder is a fake DockerRunner that captures every invocation and returns
// a scripted stdout.
type recorder struct {
	mu     sync.Mutex
	calls  []Invocation
	stdout string
}

func (r *recorder) run(ctx context.Context, inv Invocation) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, inv)
	return []byte(r.stdout), nil, nil
}

func (r *recorder) last() Invocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

func newTestManager(t *testing.T, rec *recorder) (*Manager, *vault.Store) {
	t.Helper()
	s, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return NewManager(s, WithRunner(rec.run)), s
}

func argsContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestRunBuildsExpectedArgs(t *testing.T) {
	rec := &recorder{stdout: "abc123\n"}
	m, _ := newTestManager(t, rec)

	id, err := m.Run(context.Background(), RunSpec{
		Image:  "nginx:1.27",
		Name:   "web",
		Ports:  []string{"8080:80"},
		Env:    map[string]string{"LOG_LEVEL": "debug"},
		Detach: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "abc123" {
		t.Errorf("container id = %q, want abc123", id)
	}
	inv := rec.last()
	args := inv.Args
	for _, want := range []string{"run", "-d", "--name", "web", "-p", "8080:80", "nginx:1.27"} {
		if !argsContain(args, want) {
			t.Errorf("run args %v missing %q", args, want)
		}
	}
	// Literal env reaches docker via a bare `-e NAME` flag with the value in the
	// subprocess environment — never inline as NAME=value in argv (where `ps`
	// could read it), matching the secret_env handling.
	if !argsContain(args, "-e") || !argsContain(args, "LOG_LEVEL") {
		t.Errorf("literal env flag missing from args %v", args)
	}
	if argsContain(args, "LOG_LEVEL=debug") {
		t.Errorf("literal env value leaked into argv %v", args)
	}
	if inv.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("literal env not injected into subprocess env: %v", inv.Env)
	}
}

// The whole point of secret_env: the value must reach docker via the
// subprocess environment and a bare `-e NAME` flag — never on the command
// line where `ps` (or the audit log) could see it.
func TestRunSecretEnvNeverHitsArgv(t *testing.T) {
	rec := &recorder{stdout: "id\n"}
	m, s := newTestManager(t, rec)
	if _, err := s.SetCredential("default", "db-pw", []byte("hunter2-SECRET"), nil...); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Run(context.Background(), RunSpec{
		Image:     "postgres:16",
		SecretEnv: map[string]string{"POSTGRES_PASSWORD": "db-pw"},
		Detach:    true,
	}); err != nil {
		t.Fatal(err)
	}

	inv := rec.last()
	// Bare flag, no value, in argv.
	if !argsContain(inv.Args, "-e") || !argsContain(inv.Args, "POSTGRES_PASSWORD") {
		t.Errorf("expected bare -e POSTGRES_PASSWORD in args %v", inv.Args)
	}
	// The secret value must NOT appear anywhere in argv.
	joined := strings.Join(inv.Args, " ")
	if strings.Contains(joined, "hunter2-SECRET") {
		t.Fatalf("secret value leaked into argv: %v", inv.Args)
	}
	if strings.Contains(joined, "POSTGRES_PASSWORD=") {
		t.Fatalf("secret passed as NAME=value in argv: %v", inv.Args)
	}
	// It must be in the subprocess env instead.
	if inv.Env["POSTGRES_PASSWORD"] != "hunter2-SECRET" {
		t.Errorf("secret not injected into subprocess env: %v", inv.Env)
	}
}

func TestRunMissingSecretFails(t *testing.T) {
	rec := &recorder{}
	m, _ := newTestManager(t, rec)
	_, err := m.Run(context.Background(), RunSpec{
		Image:     "alpine",
		SecretEnv: map[string]string{"TOKEN": "does-not-exist"},
		Detach:    true,
	})
	if err == nil {
		t.Fatal("expected error for missing vault secret")
	}
	if len(rec.calls) != 0 {
		t.Error("docker was invoked despite missing secret")
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	rec := &recorder{}
	m, _ := newTestManager(t, rec)
	cases := []RunSpec{
		{Image: "bad image name"},
		{Image: "alpine", Name: "../escape"},
		{Image: "alpine", Ports: []string{"not-a-port"}},
		{Image: "alpine", SecretEnv: map[string]string{"bad-name": "k"}},
	}
	for i, spec := range cases {
		if _, err := m.Run(context.Background(), spec); err == nil {
			t.Errorf("case %d: expected validation error for %+v", i, spec)
		}
	}
	if len(rec.calls) != 0 {
		t.Error("docker invoked despite invalid input")
	}
}

func TestComposeInlineYAMLViaStdin(t *testing.T) {
	rec := &recorder{}
	m, _ := newTestManager(t, rec)
	yaml := "services:\n  web:\n    image: nginx\n"
	if _, err := m.ComposeUp(context.Background(), ComposeSpec{
		Project: "stack1",
		YAML:    yaml,
	}); err != nil {
		t.Fatal(err)
	}
	inv := rec.last()
	for _, want := range []string{"compose", "-p", "stack1", "-f", "-", "up", "-d"} {
		if !argsContain(inv.Args, want) {
			t.Errorf("compose args %v missing %q", inv.Args, want)
		}
	}
	if string(inv.Stdin) != yaml {
		t.Errorf("inline yaml not piped on stdin: %q", inv.Stdin)
	}
}

func TestComposeSecretEnvInjected(t *testing.T) {
	rec := &recorder{}
	m, s := newTestManager(t, rec)
	if _, err := s.SetCredential("prod", "api-key", []byte("sk-live-xyz"), nil...); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ComposeUp(context.Background(), ComposeSpec{
		Project:   "stack2",
		YAML:      "services:\n  api:\n    image: api\n",
		SecretEnv: map[string]string{"API_KEY": "api-key"},
		Namespace: "prod",
		Env:       map[string]string{"REGION": "eu"},
	}); err != nil {
		t.Fatal(err)
	}
	inv := rec.last()
	if inv.Env["API_KEY"] != "sk-live-xyz" {
		t.Errorf("compose secret not injected: %v", inv.Env)
	}
	if inv.Env["REGION"] != "eu" {
		t.Errorf("compose literal env not injected: %v", inv.Env)
	}
	if strings.Contains(strings.Join(inv.Args, " "), "sk-live-xyz") {
		t.Errorf("compose secret leaked into argv: %v", inv.Args)
	}
}

func TestComposeValidation(t *testing.T) {
	rec := &recorder{}
	m, _ := newTestManager(t, rec)
	// Neither file nor yaml.
	if err := m.ComposeDown(context.Background(), ComposeSpec{Project: "p"}); err == nil {
		t.Error("expected error when neither file nor yaml given")
	}
	// Both file and yaml.
	if err := m.ComposeDown(context.Background(), ComposeSpec{Project: "p", File: "x.yml", YAML: "a: b"}); err == nil {
		t.Error("expected error when both file and yaml given")
	}
}

func TestListParsesJSONLines(t *testing.T) {
	rec := &recorder{stdout: `{"ID":"a1","Names":"web","Image":"nginx","State":"running","Status":"Up 2m","Ports":"80/tcp"}
{"ID":"b2","Names":"db","Image":"postgres","State":"exited","Status":"Exited (0)","Ports":""}`}
	m, _ := newTestManager(t, rec)
	list, err := m.List(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d containers, want 2", len(list))
	}
	if list[0].Name != "web" || list[0].Image != "nginx" || list[1].State != "exited" {
		t.Errorf("parsed list wrong: %+v", list)
	}
	if !argsContain(rec.last().Args, "-a") {
		t.Error("all=true did not pass -a")
	}
}

func TestComposeUpScaleFlags(t *testing.T) {
	rec := &recorder{}
	m, _ := newTestManager(t, rec)
	if _, err := m.ComposeUp(context.Background(), ComposeSpec{
		Project: "stack3",
		YAML:    "services:\n  worker:\n    image: busybox\n",
		Scale:   map[string]int{"worker": 3},
	}); err != nil {
		t.Fatal(err)
	}
	args := rec.last().Args
	// --scale must be present as worker=3 (a single argv token).
	if !argsContain(args, "--scale") || !argsContain(args, "worker=3") {
		t.Errorf("scale flags missing from %v", args)
	}
}

func TestComposePsParsesAndSurfacesEndpoints(t *testing.T) {
	rec := &recorder{stdout: `{"Name":"app-web-1","Service":"web","Image":"nginx","State":"running","Health":"healthy","Publishers":[{"PublishedPort":8080,"TargetPort":80,"Protocol":"tcp"}]}
{"Name":"app-db-1","Service":"db","Image":"postgres","State":"exited","Publishers":[]}`}
	m, _ := newTestManager(t, rec)
	svcs, err := m.ComposePs(context.Background(), ComposeSpec{Project: "app", YAML: "services: {}"})
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 2 {
		t.Fatalf("got %d services, want 2", len(svcs))
	}
	if svcs[0].Service != "web" || svcs[0].OrbHost != "app-web-1.orb.local" {
		t.Errorf("web service parsed wrong: %+v", svcs[0])
	}
	if svcs[0].Ports != "8080->80/tcp" {
		t.Errorf("port summary = %q, want 8080->80/tcp", svcs[0].Ports)
	}
	if svcs[1].State != "exited" {
		t.Errorf("stopped service not surfaced: %+v", svcs[1])
	}
}

// compose_exec must resolve the service's container via ps and then reuse the
// plain docker-exec path against that container name.
func TestComposeExecResolvesServiceAndReusesExec(t *testing.T) {
	var execTarget string
	runner := func(ctx context.Context, inv Invocation) ([]byte, []byte, error) {
		if argsContain(inv.Args, "ps") {
			return []byte(`{"Name":"app-api-1","Service":"api","State":"running"}`), nil, nil
		}
		if len(inv.Args) > 0 && inv.Args[0] == "exec" {
			execTarget = inv.Args[1] // exec <name> sh -c <cmd>
			return []byte("done"), nil, nil
		}
		return nil, nil, nil
	}
	s, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	m := NewManager(s, WithRunner(runner))

	res, err := m.ComposeExec(context.Background(), ComposeSpec{Project: "app", YAML: "x: y"}, "api", "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if execTarget != "app-api-1" {
		t.Errorf("exec targeted %q, want app-api-1", execTarget)
	}
	if res.Stdout != "done" {
		t.Errorf("exec result = %q", res.Stdout)
	}
}

func TestComposeExecUnknownServiceFails(t *testing.T) {
	rec := &recorder{stdout: `{"Name":"app-web-1","Service":"web","State":"running"}`}
	m, _ := newTestManager(t, rec)
	if _, err := m.ComposeExec(context.Background(), ComposeSpec{Project: "app", YAML: "x: y"}, "missing", "ls"); err == nil {
		t.Fatal("expected error for unknown service")
	}
}

func TestComposeBuildArgs(t *testing.T) {
	rec := &recorder{}
	m, _ := newTestManager(t, rec)
	if _, err := m.ComposeBuild(context.Background(), ComposeSpec{
		Project: "app",
		File:    "compose.yml",
	}, []string{"web", "api"}); err != nil {
		t.Fatal(err)
	}
	args := rec.last().Args
	for _, want := range []string{"compose", "-p", "app", "-f", "compose.yml", "build", "web", "api"} {
		if !argsContain(args, want) {
			t.Errorf("build args %v missing %q", args, want)
		}
	}
}

func TestBuildInlineDockerfile(t *testing.T) {
	rec := &recorder{stdout: "built\n"}
	m, _ := newTestManager(t, rec)
	df := "FROM alpine\nRUN echo hi\n"
	if _, err := m.Build(context.Background(), BuildSpec{
		Tag:         "myapp:dev",
		ContextPath: ".",
		Dockerfile:  df,
		BuildArgs:   map[string]string{"VERSION": "1.0"},
	}); err != nil {
		t.Fatal(err)
	}
	inv := rec.last()
	for _, want := range []string{"build", "-t", "myapp:dev", "-f", "-", "--build-arg", "VERSION=1.0", "."} {
		if !argsContain(inv.Args, want) {
			t.Errorf("build args %v missing %q", inv.Args, want)
		}
	}
	if string(inv.Stdin) != df {
		t.Errorf("Dockerfile not piped on stdin: %q", inv.Stdin)
	}
}
