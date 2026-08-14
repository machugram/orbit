package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/machugram/orbit/internal/container"
	"github.com/machugram/orbit/internal/keys"
	"github.com/machugram/orbit/internal/vault"
	"github.com/machugram/orbit/internal/vm"
)

func TestParseStateRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bad json", `{`, "invalid JSON"},
		{"vm missing distro", `{"vms":[{"name":"w"}]}`, "missing distro"},
		{"duplicate vm", `{"vms":[{"name":"w","distro":"alpine"},{"name":"w","distro":"ubuntu"}]}`, "duplicate vm"},
		{"compose needs file or yaml", `{"composes":[{"project":"api"}]}`, "needs file or yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseState([]byte(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseState() error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestParseStateOK(t *testing.T) {
	s, err := ParseState([]byte(`{
		"vms":[{"name":"worker","distro":"alpine"}],
		"containers":[{"image":"nginx","name":"web"}],
		"composes":[{"project":"api","yaml":"services:\n  web:\n    image: nginx"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.VMs) != 1 || s.VMs[0].Name != "worker" {
		t.Fatalf("vms = %+v", s.VMs)
	}
}

func testReconciler(t *testing.T) (*Reconciler, *vault.Store) {
	t.Helper()
	store, err := vault.Open(t.TempDir()+"/vault.db", []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	fakeOrbctl := func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		switch args[0] {
		case "create", "delete", "start", "stop":
			return nil, nil, nil
		case "info":
			return []byte(`{"name":"` + args[1] + `","state":"running"}`), nil, nil
		case "list":
			return []byte(`[]`), nil, nil
		}
		return nil, nil, nil
	}
	fakeExec := func(ctx context.Context, vmName, user, script string, args ...string) ([]byte, []byte, error) {
		return []byte("ORBIT_PROVISIONED\n"), nil, nil
	}
	fakeDocker := func(ctx context.Context, inv container.Invocation) ([]byte, []byte, error) {
		if len(inv.Args) > 0 && inv.Args[0] == "run" {
			return []byte("c0ffee\n"), nil, nil
		}
		return nil, nil, nil
	}

	km := keys.NewManager(store)
	vmm := vm.NewManager(store, vm.WithRunner(fakeOrbctl), vm.WithPollInterval(time.Millisecond), vm.WithCreateTimeout(time.Second))
	prov := vm.NewProvisioner(km, vmm, vm.WithVMExecer(fakeExec))
	cm := container.NewManager(store, container.WithRunner(fakeDocker))
	return NewReconciler(vmm, prov, cm), store
}

func TestPlanCreatesMissingAndNoopsExisting(t *testing.T) {
	r, store := testReconciler(t)
	ctx := context.Background()

	desired, err := ParseState([]byte(`{"vms":[{"name":"worker","distro":"alpine"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Plan(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionCreate {
		t.Fatalf("plan = %+v, want create worker", plan)
	}

	if err := store.CreateVM("worker", "alpine"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetVMState("worker", vault.VMRunning); err != nil {
		t.Fatal(err)
	}
	plan, err = r.Plan(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Actions[0].Kind != ActionNoop {
		t.Fatalf("plan after create = %+v, want noop", plan)
	}
}

func TestApplyThenDestroy(t *testing.T) {
	r, _ := testReconciler(t)
	ctx := context.Background()
	desired, err := ParseState([]byte(`{
		"vms":[{"name":"worker","distro":"ubuntu"}],
		"containers":[{"image":"nginx:alpine","name":"web"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}

	res, err := r.Apply(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("apply errors: %v", res.Errors)
	}
	created := 0
	for _, a := range res.Applied {
		if a.Kind == ActionCreate || a.Kind == ActionRun {
			created++
		}
	}
	if created != 2 {
		t.Fatalf("applied = %+v, want create+run", res.Applied)
	}

	gone, err := r.Destroy(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone.Errors) != 0 {
		t.Fatalf("destroy errors: %v", gone.Errors)
	}
}

func TestPlanDestroyMissingIsNoop(t *testing.T) {
	r, _ := testReconciler(t)
	desired, err := ParseState([]byte(`{"vms":[{"name":"ghost","distro":"alpine"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.PlanDestroy(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionNoop {
		t.Fatalf("plan destroy missing = %+v", plan)
	}
}
