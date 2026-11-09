package vm

import (
	"context"
	"testing"
	"time"

	"github.com/machugram/orbit/internal/vault"
)

func TestReaperDestroysExpiredEphemeral(t *testing.T) {
	f := &fakeOrb{}
	m, s := testManager(t, f)

	if err := s.CreateVM("ephem", "alpine"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVMState("ephem", vault.VMRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVMEphemeral("ephem", 60); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchVMLastUsed("ephem"); err != nil {
		t.Fatal(err)
	}
	rec, err := s.GetVM("ephem")
	if err != nil {
		t.Fatal(err)
	}

	reaper := NewReaper(m, nil).WithNow(func() time.Time {
		return time.Unix(rec.LastUsedAt+120, 0)
	})
	if err := reaper.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetVM("ephem")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != vault.VMDestroyed {
		t.Fatalf("state = %s, want destroyed", got.State)
	}
	if f.lastCall("delete") == nil {
		t.Error("expected orbctl delete")
	}
}

func TestReaperSkipsFreshAndBase(t *testing.T) {
	f := &fakeOrb{}
	m, s := testManager(t, f)

	if err := s.CreateVM("fresh", "alpine"); err != nil {
		t.Fatal(err)
	}
	_ = s.SetVMState("fresh", vault.VMRunning)
	_ = s.SetVMEphemeral("fresh", 3600)

	baseName := BaseName("alpine")
	if err := s.CreateVM(baseName, "alpine"); err != nil {
		t.Fatal(err)
	}
	_ = s.SetVMState(baseName, vault.VMRunning)
	_ = s.SetVMEphemeral(baseName, 1)

	if err := s.CreateVM("perm", "alpine"); err != nil {
		t.Fatal(err)
	}
	_ = s.SetVMState("perm", vault.VMRunning)

	now := time.Now()
	reaper := NewReaper(m, nil).WithNow(func() time.Time { return now })
	if err := reaper.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"fresh", baseName, "perm"} {
		rec, err := s.GetVM(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.State == vault.VMDestroyed {
			t.Errorf("%s was destroyed", id)
		}
	}
	if f.lastCall("delete") != nil {
		t.Error("no deletes expected")
	}
}

func TestProvisionerMarksEphemeral(t *testing.T) {
	p, s := testProvisioner(t, &fakeOrb{}, &fakeExecer{})
	rec, err := p.Create(context.Background(), "alpine", "ttl-1", true, CreateOptions{
		Ephemeral: true,
		TTL:       time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetVM(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Ephemeral || got.TTLSeconds != 60 {
		t.Fatalf("ephemeral=%v ttl=%d", got.Ephemeral, got.TTLSeconds)
	}
}
