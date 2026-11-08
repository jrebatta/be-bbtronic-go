package game

import (
	"slices"
	"time"
)

// Presence replaces WebSocketSessionRegistry and the 20 s grace timer of
// WebSocketEventListener with persisted leases (plan 7.4). Each connection
// (socket or HTTP-fallback tab) holds a lease; a member is online while any
// lease is valid. When every lease has expired the member becomes
// "reconnecting" and, after the grace period measured from that detection,
// leaves the room — exactly like Java's final disconnect.

// Connection statuses exposed in snapshots.
const (
	StatusOnline       = "online"
	StatusReconnecting = "reconnecting"
	StatusOffline      = "offline" // never connected (pure REST client)
)

// PresenceAction is the transition a sweep must apply to a member.
type PresenceAction int

// Presence actions.
const (
	PresenceNone PresenceAction = iota
	PresenceMarkReconnecting
	PresenceFinalize
)

func (m *Member) hasActiveLease(now time.Time) bool {
	for _, l := range m.Leases {
		if l.Until.After(now) {
			return true
		}
	}
	return false
}

// ConnectionStatus derives the public status from leases.
func (m *Member) ConnectionStatus(now time.Time) string {
	switch {
	case m.hasActiveLease(now):
		return StatusOnline
	case m.DisconnectedAt != nil || len(m.Leases) > 0:
		return StatusReconnecting
	}
	return StatusOffline
}

// Presence returns the transition due at now.
func (m *Member) Presence(now time.Time, grace time.Duration) PresenceAction {
	if m.hasActiveLease(now) {
		return PresenceNone
	}
	if m.DisconnectedAt != nil {
		if !now.Before(m.DisconnectedAt.Add(grace)) {
			return PresenceFinalize
		}
		return PresenceNone
	}
	if len(m.Leases) > 0 {
		return PresenceMarkReconnecting
	}
	return PresenceNone
}

// PresenceDue reports whether any member needs a presence transition.
func PresenceDue(r *Room, now time.Time, grace time.Duration) bool {
	for _, m := range r.Members {
		if m.Presence(now, grace) != PresenceNone {
			return true
		}
	}
	return false
}

// MarkReconnecting starts the grace period at detection time.
func (m *Member) MarkReconnecting(now time.Time) {
	m.Leases = nil
	t := now
	m.DisconnectedAt = &t
}

// OpenLease adds a lease, pruning expired ones and enforcing the per-player
// limit. It returns whether the public status changed (offline/reconnecting
// → online).
func (m *Member) OpenLease(l Lease, now time.Time, maxActive int) (bool, error) {
	before := m.ConnectionStatus(now)
	m.Leases = slices.DeleteFunc(m.Leases, func(x Lease) bool { return !x.Until.After(now) })
	if len(m.Leases) >= maxActive {
		return false, &Error{Code: CodeLimitReached, Message: "demasiadas conexiones activas para este jugador",
			Details: map[string]any{"maxConnections": maxActive}}
	}
	m.Leases = append(m.Leases, l)
	m.DisconnectedAt = nil
	return before != StatusOnline, nil
}

// RenewLease extends a still-valid lease. It refuses when the member is
// already in grace (the caller must re-open under the room lock).
func (m *Member) RenewLease(id string, until, now time.Time) bool {
	if m.DisconnectedAt != nil {
		return false
	}
	for i := range m.Leases {
		if m.Leases[i].ID == id && m.Leases[i].Until.After(now) {
			m.Leases[i].Until = until
			return true
		}
	}
	return false
}

// CloseLease removes a lease. If no valid lease remains the grace period
// starts. It returns whether the lease existed and whether the public status
// changed.
func (m *Member) CloseLease(id string, now time.Time) (found, statusChanged bool) {
	before := m.ConnectionStatus(now)
	n := len(m.Leases)
	m.Leases = slices.DeleteFunc(m.Leases, func(x Lease) bool { return x.ID == id })
	found = len(m.Leases) != n
	if !found {
		return false, false
	}
	if !m.hasActiveLease(now) {
		m.MarkReconnecting(now)
	}
	return true, before != m.ConnectionStatus(now)
}

// FindLease returns the lease with id.
func (m *Member) FindLease(id string) (Lease, bool) {
	for _, l := range m.Leases {
		if l.ID == id {
			return l, true
		}
	}
	return Lease{}, false
}
