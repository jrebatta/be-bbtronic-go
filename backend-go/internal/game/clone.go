package game

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// Clone returns a deep copy of the room, used to restore the aggregate when
// a command is rejected after a reconciliation that must still be committed.
func (r *Room) Clone() (*Room, error) {
	c := *r
	b, err := json.Marshal(r.State)
	if err != nil {
		return nil, fmt.Errorf("cloning state: %w", err)
	}
	c.State = State{}
	if err := json.Unmarshal(b, &c.State); err != nil {
		return nil, fmt.Errorf("cloning state: %w", err)
	}
	if r.HostID != nil {
		h := *r.HostID
		c.HostID = &h
	}
	c.Members = make([]*Member, len(r.Members))
	for i, m := range r.Members {
		mc := *m
		mc.Leases = slices.Clone(m.Leases)
		if m.DisconnectedAt != nil {
			t := *m.DisconnectedAt
			mc.DisconnectedAt = &t
		}
		c.Members[i] = &mc
	}
	if r.Direct != nil {
		c.Direct = &DirectRound{Questions: slices.Clone(r.Direct.Questions), Shown: maps.Clone(r.Direct.Shown)}
	}
	return &c, nil
}

// SameMemberState reports whether two snapshots of a member are equal in
// every persisted mutable column.
func SameMemberState(a, b *Member) bool {
	if a.Ready != b.Ready || len(a.Leases) != len(b.Leases) {
		return false
	}
	if (a.DisconnectedAt == nil) != (b.DisconnectedAt == nil) ||
		(a.DisconnectedAt != nil && !a.DisconnectedAt.Equal(*b.DisconnectedAt)) {
		return false
	}
	for i := range a.Leases {
		if a.Leases[i].ID != b.Leases[i].ID || a.Leases[i].Kind != b.Leases[i].Kind || !a.Leases[i].Until.Equal(b.Leases[i].Until) {
			return false
		}
	}
	return true
}
