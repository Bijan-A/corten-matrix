package connector

import (
	"reflect"
	"testing"
)

func TestPlanGroupConsolidation(t *testing.T) {
	comma := "tel:+15551111111,tel:+15552222222"
	commaB := "tel:+15553333333,tel:+15554444444"

	tests := []struct {
		name          string
		entries       []groupConsolidationEntry
		wantRoom      []groupConsolidationGroup
		wantRows      []groupRowReKey
		wantAmbiguous []string
	}{
		{
			name: "multiple gid encodings collapse to one canonical group",
			entries: []groupConsolidationEntry{
				{cloudChatID: "c1", portalID: "gid:aaaa", canonical: comma},
				{cloudChatID: "c2", portalID: "gid:bbbb", canonical: comma},
				{cloudChatID: "c3", portalID: "gid:cccc", canonical: comma},
			},
			wantRoom: []groupConsolidationGroup{
				{canonical: comma, members: []string{"gid:aaaa", "gid:bbbb", "gid:cccc"}},
			},
			wantRows: []groupRowReKey{
				{cloudChatID: "c1", from: "gid:aaaa", to: comma, carryOrphans: true},
				{cloudChatID: "c2", from: "gid:bbbb", to: comma, carryOrphans: true},
				{cloudChatID: "c3", from: "gid:cccc", to: comma, carryOrphans: true},
			},
		},
		{
			name: "single gid still re-keyed to participant form",
			entries: []groupConsolidationEntry{
				{cloudChatID: "c1", portalID: "gid:aaaa", canonical: comma},
			},
			wantRoom: []groupConsolidationGroup{
				{canonical: comma, members: []string{"gid:aaaa"}},
			},
			wantRows: []groupRowReKey{{cloudChatID: "c1", from: "gid:aaaa", to: comma, carryOrphans: true}},
		},
		{
			name: "already-canonical single portal is a no-op",
			entries: []groupConsolidationEntry{
				{cloudChatID: "c1", portalID: comma, canonical: comma},
			},
		},
		{
			// The canonical portal itself needs no room move (moveGroupRooms
			// skips it), so only the duplicate is listed as a member.
			name: "comma survivor plus gid duplicate still consolidates",
			entries: []groupConsolidationEntry{
				{cloudChatID: "c1", portalID: comma, canonical: comma},
				{cloudChatID: "c2", portalID: "gid:aaaa", canonical: comma},
			},
			wantRoom: []groupConsolidationGroup{
				{canonical: comma, members: []string{"gid:aaaa"}},
			},
			wantRows: []groupRowReKey{{cloudChatID: "c2", from: "gid:aaaa", to: comma, carryOrphans: true}},
		},
		{
			name: "distinct participant sets stay separate",
			entries: []groupConsolidationEntry{
				{cloudChatID: "c1", portalID: "gid:aaaa", canonical: comma},
				{cloudChatID: "c2", portalID: "gid:bbbb", canonical: comma},
				{cloudChatID: "c3", portalID: "gid:cccc", canonical: commaB},
				{cloudChatID: "c4", portalID: "gid:dddd", canonical: commaB},
			},
			wantRoom: []groupConsolidationGroup{
				{canonical: comma, members: []string{"gid:aaaa", "gid:bbbb"}},
				{canonical: commaB, members: []string{"gid:cccc", "gid:dddd"}},
			},
			wantRows: []groupRowReKey{
				{cloudChatID: "c1", from: "gid:aaaa", to: comma, carryOrphans: true},
				{cloudChatID: "c2", from: "gid:bbbb", to: comma, carryOrphans: true},
				{cloudChatID: "c3", from: "gid:cccc", to: commaB, carryOrphans: true},
				{cloudChatID: "c4", from: "gid:dddd", to: commaB, carryOrphans: true},
			},
		},
		{
			name: "two conversations under one portal, one already matching: room stays put",
			// The regression case. Two chats share a portal ID (identical
			// rosters once, per the one-room-per-participant-set design) and one
			// roster has since changed. Old behavior: the room was re-ID'd to
			// the changed roster's key and BOTH rows were dragged along, so the
			// unchanged row demanded its key back next startup — the room
			// ping-ponged between two rosters on every restart, kicking and
			// re-inviting the difference. Now the room stays where a row still
			// claims it and only the diverged conversation splits off.
			entries: []groupConsolidationEntry{
				{cloudChatID: "stay", portalID: comma, canonical: comma},
				{cloudChatID: "diverged", portalID: comma, canonical: commaB},
			},
			// carryOrphans stays false: a conversation still claims this portal,
			// so chat_id-less rows can't be attributed to the one leaving.
			wantRows: []groupRowReKey{{cloudChatID: "diverged", from: comma, to: commaB}},
		},
		{
			name: "two conversations under one portal, neither matching: room stays put",
			// Both rosters changed and no row claims the current key, so there
			// is no target that describes what the room holds. Picking the
			// lowest would be stable but still a guess — the shape #23 removed
			// from orphanedGroupRoomPortalIDs. The rows still move; the room is
			// reported instead.
			entries: []groupConsolidationEntry{
				{cloudChatID: "c1", portalID: "gid:aaaa", canonical: commaB},
				{cloudChatID: "c2", portalID: "gid:aaaa", canonical: comma},
			},
			wantAmbiguous: []string{"gid:aaaa"},
			wantRows: []groupRowReKey{
				{cloudChatID: "c1", from: "gid:aaaa", to: commaB},
				{cloudChatID: "c2", from: "gid:aaaa", to: comma},
			},
		},
		{
			name: "duplicate portal IDs are de-duplicated within a group",
			entries: []groupConsolidationEntry{
				{cloudChatID: "c1", portalID: "gid:aaaa", canonical: comma},
				{cloudChatID: "c1", portalID: "gid:aaaa", canonical: comma},
				{cloudChatID: "c2", portalID: "gid:bbbb", canonical: comma},
			},
			wantRoom: []groupConsolidationGroup{
				{canonical: comma, members: []string{"gid:aaaa", "gid:bbbb"}},
			},
			wantRows: []groupRowReKey{
				{cloudChatID: "c1", from: "gid:aaaa", to: comma, carryOrphans: true},
				{cloudChatID: "c1", from: "gid:aaaa", to: comma, carryOrphans: true},
				{cloudChatID: "c2", from: "gid:bbbb", to: comma, carryOrphans: true},
			},
		},
		{
			name: "entries with empty canonical or portal are ignored",
			entries: []groupConsolidationEntry{
				{cloudChatID: "c1", portalID: "gid:aaaa", canonical: ""},
				{cloudChatID: "c2", portalID: "", canonical: comma},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planGroupConsolidation(tt.entries)
			if !reflect.DeepEqual(got.roomGroups, tt.wantRoom) {
				t.Errorf("roomGroups = %#v, want %#v", got.roomGroups, tt.wantRoom)
			}
			if !reflect.DeepEqual(got.rowReKeys, tt.wantRows) {
				t.Errorf("rowReKeys = %#v, want %#v", got.rowReKeys, tt.wantRows)
			}
			if !reflect.DeepEqual(got.ambiguousRooms, tt.wantAmbiguous) {
				t.Errorf("ambiguousRooms = %#v, want %#v", got.ambiguousRooms, tt.wantAmbiguous)
			}
		})
	}
}

// TestPlanGroupConsolidationConverges is the property the old planner lacked:
// applying the plan and re-planning must reach a fixed point, so consolidation
// cannot run (and move rooms) on every startup forever.
func TestPlanGroupConsolidationConverges(t *testing.T) {
	comma := "tel:+15551111111,tel:+15552222222"
	commaB := "tel:+15551111111,tel:+15552222222,tel:+15559999999"

	// Start from the observed bad state: two conversations, one portal, rosters
	// disagreeing.
	entries := []groupConsolidationEntry{
		{cloudChatID: "stay", portalID: comma, canonical: comma},
		{cloudChatID: "diverged", portalID: comma, canonical: commaB},
	}

	for round := 1; round <= 4; round++ {
		plan := planGroupConsolidation(entries)
		if round > 1 {
			if len(plan.rowReKeys) != 0 || len(plan.roomGroups) != 0 {
				t.Fatalf("round %d still has work to do: %#v — consolidation is oscillating", round, plan)
			}
			continue
		}
		// Apply round 1's re-keys the way reKeyGroupCloudRows does: per row.
		applied := make([]groupConsolidationEntry, 0, len(entries))
		for _, e := range entries {
			for _, rk := range plan.rowReKeys {
				if rk.cloudChatID == e.cloudChatID {
					e.portalID = rk.to
				}
			}
			applied = append(applied, e)
		}
		entries = applied
	}
}

// TestMergeOrphanedGroupRooms guards the issue-#9 fix: a group deferred by
// groupRoomMoveBudgetPerStartup gets its cloud rows re-keyed to canonical on
// the SAME startup (reKeyGroupCloudRows runs unbudgeted), so on the NEXT
// startup planGroupConsolidation no longer sees it — its cloud_chat rows
// already present a single canonical key. mergeOrphanedGroupRooms folds the
// orphaned gid: room (found by orphanedGroupRoomPortalIDs, keyed by the
// group's stable group_id rather than its mutable portal_id) back into the
// plan so moveGroupRooms still re-IDs/tombstones it.
func TestMergeOrphanedGroupRooms(t *testing.T) {
	const comma = "tel:+15551111111,tel:+15552222222"
	const commaB = "tel:+15553333333,tel:+15554444444"

	tests := []struct {
		name    string
		groups  []groupConsolidationGroup
		orphans map[string]string
		want    []groupConsolidationGroup
	}{
		{
			name:    "no orphans leaves the plan untouched",
			groups:  []groupConsolidationGroup{{canonical: comma, members: []string{"gid:aaaa"}}},
			orphans: map[string]string{},
			want:    []groupConsolidationGroup{{canonical: comma, members: []string{"gid:aaaa"}}},
		},
		{
			// The deferred-then-orphaned case this issue is about: the
			// participant-key path found nothing (the group's cloud rows are
			// already canonical), but its old gid: room is still out there.
			name:    "orphan with no matching group creates a new one",
			groups:  nil,
			orphans: map[string]string{"gid:stale": comma},
			want:    []groupConsolidationGroup{{canonical: comma, members: []string{"gid:stale"}}},
		},
		{
			name:    "orphan folds into an existing group for the same canonical",
			groups:  []groupConsolidationGroup{{canonical: comma, members: []string{"gid:aaaa"}}},
			orphans: map[string]string{"gid:stale": comma},
			want:    []groupConsolidationGroup{{canonical: comma, members: []string{"gid:aaaa", "gid:stale"}}},
		},
		{
			name:    "orphan already present as a member is not duplicated",
			groups:  []groupConsolidationGroup{{canonical: comma, members: []string{"gid:aaaa", "gid:stale"}}},
			orphans: map[string]string{"gid:stale": comma},
			want:    []groupConsolidationGroup{{canonical: comma, members: []string{"gid:aaaa", "gid:stale"}}},
		},
		{
			name: "mixed: one folds in, one creates a new group, sorted by canonical",
			groups: []groupConsolidationGroup{
				{canonical: commaB, members: []string{"gid:bbbb"}},
			},
			orphans: map[string]string{
				"gid:stale-b": commaB,
				"gid:stale-a": comma,
			},
			want: []groupConsolidationGroup{
				{canonical: comma, members: []string{"gid:stale-a"}},
				{canonical: commaB, members: []string{"gid:bbbb", "gid:stale-b"}},
			},
		},
		{
			name:    "an orphan mapped to itself (no stale room) is ignored",
			groups:  nil,
			orphans: map[string]string{comma: comma},
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeOrphanedGroupRooms(tt.groups, tt.orphans)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("mergeOrphanedGroupRooms() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestPlanGroupRoomMoves(t *testing.T) {
	const comma = "tel:+15551111111,tel:+15552222222"

	tests := []struct {
		name             string
		canonicalHasRoom bool
		members          []groupMemberRoom
		want             groupRoomMovePlan
	}{
		{
			// Canonical key already has a room: it must survive, and even a larger
			// gid: room tombstones into it (ReIDPortal would otherwise tombstone the
			// larger room — the bug this guards against).
			name:             "existing canonical room survives over a larger member",
			canonicalHasRoom: true,
			members: []groupMemberRoom{
				{portalID: "gid:aaaa", hasRoom: true, msgCount: 9999},
			},
			want: groupRoomMovePlan{survivor: comma, reIDs: []string{"gid:aaaa"}},
		},
		{
			name:             "no canonical room: most-history member is renamed onto canonical first",
			canonicalHasRoom: false,
			members: []groupMemberRoom{
				{portalID: "gid:aaaa", hasRoom: true, msgCount: 10},
				{portalID: "gid:bbbb", hasRoom: true, msgCount: 50},
				{portalID: "gid:cccc", hasRoom: true, msgCount: 20},
			},
			want: groupRoomMovePlan{survivor: "gid:bbbb", reIDs: []string{"gid:bbbb", "gid:aaaa", "gid:cccc"}},
		},
		{
			name:             "members without rooms are skipped",
			canonicalHasRoom: false,
			members: []groupMemberRoom{
				{portalID: "gid:aaaa", hasRoom: false},
				{portalID: "gid:bbbb", hasRoom: true, msgCount: 5},
				{portalID: "gid:cccc", hasRoom: false},
			},
			want: groupRoomMovePlan{survivor: "gid:bbbb", reIDs: []string{"gid:bbbb"}},
		},
		{
			name:             "no member has a room: nothing to move, createPortals builds it",
			canonicalHasRoom: false,
			members: []groupMemberRoom{
				{portalID: "gid:aaaa", hasRoom: false},
				{portalID: "gid:bbbb", hasRoom: false},
			},
			want: groupRoomMovePlan{survivor: "", reIDs: nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planGroupRoomMoves(comma, tt.canonicalHasRoom, tt.members)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("planGroupRoomMoves() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestPlanGroupConsolidationAmbiguousRoomConverges: leaving an ambiguous room in
// place must still settle. The conversations move to their own keys, so the next
// plan has nothing to say about that portal at all — otherwise "don't guess"
// would trade a misfiled room for a pass that reports the same room forever.
func TestPlanGroupConsolidationAmbiguousRoomConverges(t *testing.T) {
	const comma = "tel:+15551111111,tel:+15552222222"
	const commaB = "tel:+15553333333,tel:+15554444444"
	entries := []groupConsolidationEntry{
		{cloudChatID: "c1", portalID: "gid:aaaa", canonical: comma},
		{cloudChatID: "c2", portalID: "gid:aaaa", canonical: commaB},
	}

	first := planGroupConsolidation(entries)
	if len(first.ambiguousRooms) != 1 || first.ambiguousRooms[0] != "gid:aaaa" {
		t.Fatalf("first pass ambiguousRooms = %#v, want the shared portal", first.ambiguousRooms)
	}
	if len(first.roomGroups) != 0 {
		t.Errorf("first pass moved a room: %#v", first.roomGroups)
	}
	if len(first.rowReKeys) != 2 {
		t.Fatalf("first pass rowReKeys = %#v, want both conversations re-keyed", first.rowReKeys)
	}

	// Apply the re-keys the way reKeyGroupCloudRows does, then re-plan.
	applied := make([]groupConsolidationEntry, 0, len(entries))
	for _, e := range entries {
		for _, rk := range first.rowReKeys {
			if rk.cloudChatID == e.cloudChatID {
				e.portalID = rk.to
			}
		}
		applied = append(applied, e)
	}
	second := planGroupConsolidation(applied)
	if len(second.rowReKeys) != 0 || len(second.roomGroups) != 0 || len(second.ambiguousRooms) != 0 {
		t.Errorf("second pass still has work: %#v", second)
	}
}
