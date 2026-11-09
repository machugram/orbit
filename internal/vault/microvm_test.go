package vault

import (
	"errors"
	"testing"
)

func TestMicroVMLifecycle(t *testing.T) {
	s, _ := openTestStore(t)

	if _, err := s.GetMicroVM("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMicroVM(missing) = %v, want ErrNotFound", err)
	}

	rec := MicroVM{
		ID:      "uvm-1",
		Carrier: "carrier-a",
		Image:   "ubuntu",
		KeyID:   "key-123",
		SSHPort: 23456,
		Handle:  "/var/lib/orbit-microvm/uvm-1",
	}
	if err := s.CreateMicroVM(rec); err != nil {
		t.Fatal(err)
	}
	// Duplicate id is rejected by the primary key.
	if err := s.CreateMicroVM(rec); err == nil {
		t.Fatal("CreateMicroVM(duplicate) succeeded, want error")
	}

	got, err := s.GetMicroVM("uvm-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Carrier != "carrier-a" || got.Image != "ubuntu" || got.KeyID != "key-123" ||
		got.SSHPort != 23456 || got.Handle != "/var/lib/orbit-microvm/uvm-1" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.State != VMCreating {
		t.Errorf("initial state = %q, want %q", got.State, VMCreating)
	}
	if got.CreatedAt == 0 {
		t.Error("CreatedAt not set")
	}

	if err := s.SetMicroVMState("uvm-1", VMRunning); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetMicroVM("uvm-1"); got.State != VMRunning {
		t.Errorf("state = %q, want running", got.State)
	}

	// Destroying records destroyed_at.
	if err := s.SetMicroVMState("uvm-1", VMDestroyed); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMicroVM("uvm-1")
	if got.State != VMDestroyed || got.DestroyedAt == 0 {
		t.Errorf("after destroy: state=%q destroyedAt=%d", got.State, got.DestroyedAt)
	}

	again := MicroVM{
		ID: "uvm-1", Carrier: "carrier-b", Image: "alpine",
		KeyID: "key-new", SSHPort: 24000, Handle: "/var/lib/orbit-microvm/uvm-1b",
	}
	if err := s.CreateMicroVM(again); err != nil {
		t.Fatalf("recreate after destroy: %v", err)
	}
	got, _ = s.GetMicroVM("uvm-1")
	if got.State != VMCreating || got.Carrier != "carrier-b" || got.KeyID != "key-new" ||
		got.SSHPort != 24000 || got.DestroyedAt != 0 {
		t.Errorf("recreated microvm kept tombstone fields: %+v", got)
	}
	if err := s.CreateMicroVM(again); err == nil {
		t.Fatal("live duplicate name accepted")
	}

	if err := s.SetMicroVMState("ghost", VMStopped); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetMicroVMState(missing) = %v, want ErrNotFound", err)
	}
}

func TestListMicroVMs(t *testing.T) {
	s, _ := openTestStore(t)
	if vms, err := s.ListMicroVMs(); err != nil || len(vms) != 0 {
		t.Fatalf("empty list: %v %v", vms, err)
	}
	for _, id := range []string{"uvm-a", "uvm-b"} {
		if err := s.CreateMicroVM(MicroVM{ID: id, Carrier: "c", Image: "ubuntu", SSHPort: 2222}); err != nil {
			t.Fatal(err)
		}
	}
	vms, err := s.ListMicroVMs()
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 {
		t.Fatalf("len = %d, want 2", len(vms))
	}
}
