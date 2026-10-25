package bridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newDegradedRegistry(t *testing.T) (*Registry, *time.Time) {
	t.Helper()
	now := time.Now()
	reg, err := NewRegistry(Config{
		StateDir: t.TempDir(),
		Login:    nopLogin{},
		Factory:  nopFactory{},
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return reg, &now
}

func TestDegradedMarkedOnSuspendError(t *testing.T) {
	reg, _ := newDegradedRegistry(t)
	suspend := errors.New("owa: CreateItem: ErrorAccountSuspendShowTierUpgrade: Account suspended")
	if err := reg.Call(context.Background(), "user@example.com", func(context.Context, Provider) error {
		return suspend
	}); err == nil {
		t.Fatal("suspension error swallowed")
	}
	if _, degraded := reg.DegradedReason("user@example.com"); !degraded {
		t.Fatal("account not marked degraded")
	}
	// The reason text reaches the client verbatim.
	if reason, _ := reg.DegradedReason("user@example.com"); reason == "" {
		t.Fatal("empty degraded reason")
	}
}

func TestDegradedClearedOnSuccess(t *testing.T) {
	reg, _ := newDegradedRegistry(t)
	suspend := errors.New("owa: CreateItem: ErrorAccountSuspendShowTierUpgrade: Account suspended")
	_ = reg.Call(context.Background(), "user@example.com", func(context.Context, Provider) error {
		return suspend
	})
	if err := reg.Call(context.Background(), "user@example.com", func(context.Context, Provider) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, degraded := reg.DegradedReason("user@example.com"); degraded {
		t.Fatal("degraded state survived a successful call")
	}
}

func TestDegradedOtherErrorsDoNotMark(t *testing.T) {
	reg, _ := newDegradedRegistry(t)
	_ = reg.Call(context.Background(), "user@example.com", func(context.Context, Provider) error {
		return errors.New("owa: FindItem: something else")
	})
	if _, degraded := reg.DegradedReason("user@example.com"); degraded {
		t.Fatal("unrelated error marked account degraded")
	}
}

func TestDegradedProbeAfterInterval(t *testing.T) {
	reg, now := newDegradedRegistry(t)
	suspend := errors.New("owa: CreateItem: ErrorAccountSuspendShowTierUpgrade: Account suspended")
	_ = reg.Call(context.Background(), "user@example.com", func(context.Context, Provider) error {
		return suspend
	})
	*now = now.Add(degradedProbeAfter + time.Second)
	if _, degraded := reg.DegradedReason("user@example.com"); degraded {
		t.Fatal("fail-fast outlived the probe interval")
	}
}

type nopLogin struct{}

func (nopLogin) Login(_ context.Context, _ string, _ Channel) (Session, error) {
	return Session{}, nil
}
func (nopLogin) Renew(_ context.Context, _ string, s Session) (Session, error) {
	return s, nil
}

type nopFactory struct{}

func (nopFactory) New(_ string, _ Session) Provider { return nil }
