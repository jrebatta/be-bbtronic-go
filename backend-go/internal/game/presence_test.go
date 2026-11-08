package game

import (
	"testing"
	"time"
)

func TestMember_PresenceLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	grace := 20 * time.Second
	m := &Member{ID: 1}
	if m.ConnectionStatus(now) != StatusOffline || m.Presence(now, grace) != PresenceNone {
		t.Fatal("never connected: offline, no action")
	}
	changed, err := m.OpenLease(Lease{ID: "a", Until: now.Add(45 * time.Second)}, now, 2)
	if err != nil || !changed || m.ConnectionStatus(now) != StatusOnline {
		t.Fatalf("open: changed=%t err=%v", changed, err)
	}
	if changed, _ := m.OpenLease(Lease{ID: "b", Until: now.Add(45 * time.Second)}, now, 2); changed {
		t.Fatal("a second tab does not change the visible status")
	}
	if _, err := m.OpenLease(Lease{ID: "c", Until: now.Add(45 * time.Second)}, now, 2); !IsCode(err, CodeLimitReached) {
		t.Fatalf("limit: %v", err)
	}
	if found, changed := m.CloseLease("a", now); !found || changed {
		t.Fatal("closing one of two tabs keeps the member online")
	}
	if !m.RenewLease("b", now.Add(90*time.Second), now) || m.RenewLease("zzz", now, now) {
		t.Fatal("renew")
	}

	later := now.Add(100 * time.Second) // lease b expired silently
	if m.ConnectionStatus(later) != StatusReconnecting || m.Presence(later, grace) != PresenceMarkReconnecting {
		t.Fatal("silent expiry must be detected")
	}
	m.MarkReconnecting(later)
	if m.Presence(later.Add(19*time.Second), grace) != PresenceNone {
		t.Fatal("grace is measured from detection")
	}
	if m.Presence(later.Add(20*time.Second), grace) != PresenceFinalize {
		t.Fatal("grace elapsed: finalize")
	}
	if m.RenewLease("b", later.Add(time.Minute), later) {
		t.Fatal("renewal during grace must go through a full re-open")
	}
	if changed, err := m.OpenLease(Lease{ID: "d", Until: later.Add(45 * time.Second)}, later, 2); err != nil || !changed || m.DisconnectedAt != nil {
		t.Fatal("reconnect during grace cancels the departure")
	}
}

func TestMember_CloseLastLeaseStartsGrace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	m := &Member{ID: 1}
	_, _ = m.OpenLease(Lease{ID: "a", Until: now.Add(time.Minute)}, now, 4)
	found, changed := m.CloseLease("a", now)
	if !found || !changed || m.DisconnectedAt == nil || m.ConnectionStatus(now) != StatusReconnecting {
		t.Fatal("closing the last connection starts the grace period (Java userReconnecting)")
	}
}
