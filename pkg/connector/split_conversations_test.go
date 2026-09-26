// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for the split-conversation diagnostic.

package connector

import (
	"context"
	"strings"
	"testing"
)

// TestGroupSplitConversations pins what counts as "split".
//
// The distinction that matters: several cloud_chat rows sharing ONE portal is
// the intended one-room-per-participant-set design and must not be reported,
// while one group_id spread across two portals is the actual defect.
func TestGroupSplitConversations(t *testing.T) {
	rows := []splitConversationRow{
		// Split: one conversation, two handle-keyed rooms (phone vs email).
		{GroupID: "g-direct", PortalID: "tel:+15551230000"},
		{GroupID: "g-direct", PortalID: "mailto:sam@example.com"},
		// Not split: two chats legitimately sharing one portal.
		{GroupID: "g-shared", PortalID: "tel:+1,tel:+2"},
		{GroupID: "g-shared", PortalID: "tel:+1,tel:+2"},
		// Not split: a single whole conversation.
		{GroupID: "g-whole", PortalID: "gid:abcd"},
		// Skipped: incomplete rows carry no signal.
		{GroupID: "", PortalID: "tel:+15551239999"},
		{GroupID: "g-empty-portal", PortalID: ""},
	}

	got := groupSplitConversations(rows)
	if len(got) != 1 {
		t.Fatalf("groupSplitConversations() returned %d conversations, want 1: %+v", len(got), got)
	}
	if got[0].GroupID != "g-direct" {
		t.Errorf("group_id = %q, want %q", got[0].GroupID, "g-direct")
	}
	if len(got[0].Shards) != 2 {
		t.Fatalf("shards = %d, want 2: %+v", len(got[0].Shards), got[0].Shards)
	}
}

// TestSplitConversationClassification covers the two questions the report is
// built on: is this a direct chat, and would merging it need a room rebuild.
func TestSplitConversationClassification(t *testing.T) {
	cases := []struct {
		name         string
		shards       []splitConversationShard
		wantDirect   bool
		wantRebuild  bool
		wantPopuland int
	}{
		{
			name: "direct chat partitioned across phone and email",
			shards: []splitConversationShard{
				{PortalID: "tel:+15551230000", Messages: 6916},
				{PortalID: "mailto:sam@example.com", Messages: 3125},
			},
			wantDirect: true, wantRebuild: true, wantPopuland: 2,
		},
		{
			name: "group with one populated room and an empty shell",
			shards: []splitConversationShard{
				{PortalID: "gid:abcd", Messages: 812},
				{PortalID: "tel:+1,tel:+2", Messages: 0},
			},
			wantDirect: false, wantRebuild: false, wantPopuland: 1,
		},
		{
			name: "comma-keyed participant set is a group, not a direct chat",
			shards: []splitConversationShard{
				{PortalID: "tel:+1,tel:+2", Messages: 5},
				{PortalID: "tel:+1,tel:+3", Messages: 0},
			},
			wantDirect: false, wantRebuild: false, wantPopuland: 1,
		},
		{
			// Both empty: nothing to lose, so no rebuild is needed.
			name: "two empty shells",
			shards: []splitConversationShard{
				{PortalID: "gid:abcd", Messages: 0},
				{PortalID: "gid:efgh", Messages: 0},
			},
			wantDirect: false, wantRebuild: false, wantPopuland: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			split := splitConversation{GroupID: "g", Shards: tc.shards}
			if got := split.IsDirect(); got != tc.wantDirect {
				t.Errorf("IsDirect() = %v, want %v", got, tc.wantDirect)
			}
			if got := split.NeedsRebuild(); got != tc.wantRebuild {
				t.Errorf("NeedsRebuild() = %v, want %v", got, tc.wantRebuild)
			}
			if got := split.PopulatedShards(); got != tc.wantPopuland {
				t.Errorf("PopulatedShards() = %d, want %d", got, tc.wantPopuland)
			}
		})
	}
}

// TestInPlaceholders covers the offset numbering and the empty guard.
//
// The empty case matters: "IN ()" is a syntax error on both dialects, so the
// builder has to report that rather than emit it.
func TestInPlaceholders(t *testing.T) {
	clause, args, ok := inPlaceholders([]string{"a", "b", "c"}, 2)
	if !ok {
		t.Fatal("inPlaceholders() ok = false, want true")
	}
	if clause != "($2, $3, $4)" {
		t.Errorf("clause = %q, want %q", clause, "($2, $3, $4)")
	}
	if len(args) != 3 {
		t.Errorf("args = %v, want 3 values", args)
	}

	// Duplicates collapse, and the numbering stays dense across the collapse.
	clause, args, ok = inPlaceholders([]string{"a", "a", "b"}, 1)
	if !ok || clause != "($1, $2)" || len(args) != 2 {
		t.Errorf("dedupe: clause = %q, args = %v, ok = %v; want ($1, $2) with 2 args", clause, args, ok)
	}

	if _, _, ok := inPlaceholders(nil, 1); ok {
		t.Error("inPlaceholders(nil) ok = true, want false so the caller skips the query")
	}
}

// TestFormatSplitConversations checks the report separates the expensive cases
// from the cheap ones, since that split is the point of running it.
func TestFormatSplitConversations(t *testing.T) {
	if got := formatSplitConversations(splitConversationReport{}); !strings.Contains(got, "No split conversations") {
		t.Errorf("empty report = %q, want the all-clear wording", got)
	}

	report := formatSplitConversations(splitConversationReport{Splits: []splitConversation{
		{GroupID: "g-partitioned", Shards: []splitConversationShard{
			{PortalID: "tel:+15551230000", Messages: 6916, MXID: "!a:hs"},
			{PortalID: "mailto:sam@example.com", Messages: 3125, MXID: "!b:hs"},
		}},
		{GroupID: "g-shell", Shards: []splitConversationShard{
			{PortalID: "gid:abcd", Messages: 812, MXID: "!c:hs"},
			{PortalID: "tel:+1,tel:+2", Messages: 0},
		}},
	}})
	for _, want := range []string{
		"2 conversation(s)", "Partitioned — 1", "Empty duplicates — 1",
		"g-partitioned", "g-shell", "6916", "no room", "(direct)", "(group)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

// TestFindSplitConversationsRunsOnSQLite executes the real statements against
// SQLite rather than asserting on their text.
//
// This is the shape that catches a dialect bug: a Postgres-only construct
// (`= ANY(...)`, say) parses fine as a Go string and would sail past any
// text-based assertion, while SQLite rejects it at prepare time. The query also
// has to survive the ordinary case of several chats sharing one portal without
// reporting it as a split.
func TestFindSplitConversationsRunsOnSQLite(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	const bridgeID = "" // matches production single-bridge (portal.bridge_id='')
	const now = int64(1_700_000_000_000)

	if _, err := db.Exec(ctx, `CREATE TABLE portal (bridge_id TEXT, id TEXT, receiver TEXT, mxid TEXT)`); err != nil {
		t.Fatalf("create portal table: %v", err)
	}
	chat := func(cid, groupID, portalID string) {
		if _, err := db.Exec(ctx,
			`INSERT INTO cloud_chat (login_id, cloud_chat_id, group_id, portal_id, created_ts, is_filtered, deleted) VALUES ($1,$2,$3,$4,$5,0,0)`,
			testSQLLoginID, cid, groupID, portalID, now); err != nil {
			t.Fatalf("insert chat %s: %v", cid, err)
		}
	}
	filteredChat := func(cid, groupID, portalID string) {
		if _, err := db.Exec(ctx,
			`INSERT INTO cloud_chat (login_id, cloud_chat_id, group_id, portal_id, created_ts, is_filtered, deleted) VALUES ($1,$2,$3,$4,$5,1,0)`,
			testSQLLoginID, cid, groupID, portalID, now); err != nil {
			t.Fatalf("insert filtered chat %s: %v", cid, err)
		}
	}
	room := func(portalID string) {
		if _, err := db.Exec(ctx,
			`INSERT INTO portal (bridge_id, id, receiver, mxid) VALUES ($1,$2,$3,$4)`,
			bridgeID, portalID, string(testSQLLoginID), "!room-"+portalID+":hs"); err != nil {
			t.Fatalf("insert portal %s: %v", portalID, err)
		}
	}
	msg := func(guid, portalID string, deleted int) {
		if _, err := db.Exec(ctx,
			`INSERT INTO cloud_message (login_id, guid, portal_id, timestamp_ms, is_from_me, deleted, created_ts, updated_ts)
			 VALUES ($1,$2,$3,$4,0,$5,$4,$4)`,
			testSQLLoginID, guid, portalID, now, deleted); err != nil {
			t.Fatalf("insert message %s: %v", guid, err)
		}
	}

	// The defect: one conversation, two handle-keyed rooms, content partitioned.
	chat("c-phone", "g-direct", "tel:+15551230000")
	chat("c-email", "g-direct", "mailto:sam@example.com")
	room("tel:+15551230000")
	room("mailto:sam@example.com")
	msg("m1", "tel:+15551230000", 0)
	msg("m2", "tel:+15551230000", 0)
	msg("m3", "mailto:sam@example.com", 0)
	// Deleted rows must not count toward a shard being "populated".
	msg("m4", "mailto:sam@example.com", 1)

	// Not a split: two chats sharing one portal, which is the intended design.
	chat("c-share-a", "g-shared", "tel:+1,tel:+2")
	chat("c-share-b", "g-shared", "tel:+1,tel:+2")
	room("tel:+1,tel:+2")

	// Not a split: a single whole conversation.
	chat("c-whole", "g-whole", "gid:abcd")
	room("gid:abcd")

	// Not a split either: the second portal's only chat is iCloud-filtered, so
	// with bridge_filtered_chats off the bridge never gives it a room. Counting
	// it as a duplicate of the bridged one would be a false positive.
	chat("c-known", "g-filtered", "tel:+15559990000")
	room("tel:+15559990000")
	filteredChat("c-junk", "g-filtered", "tel:+15559991111")

	got, err := store.findSplitConversations(ctx, bridgeID, false)
	if err != nil {
		t.Fatalf("findSplitConversations: %v", err)
	}
	if got.FilteredPortalsExcluded != 1 {
		t.Errorf("FilteredPortalsExcluded = %d, want 1", got.FilteredPortalsExcluded)
	}
	if len(got.Splits) != 1 {
		t.Fatalf("found %d split conversations, want 1: %+v", len(got.Splits), got.Splits)
	}
	split := got.Splits[0]
	if split.GroupID != "g-direct" {
		t.Errorf("group_id = %q, want g-direct", split.GroupID)
	}
	if !split.IsDirect() {
		t.Error("IsDirect() = false, want true for two handle-keyed portals")
	}
	if !split.NeedsRebuild() {
		t.Error("NeedsRebuild() = false, want true when both shards hold messages")
	}
	// Most-populated first, and the deleted row must not be counted.
	if len(split.Shards) != 2 {
		t.Fatalf("shards = %d, want 2", len(split.Shards))
	}
	if split.Shards[0].PortalID != "tel:+15551230000" || split.Shards[0].Messages != 2 {
		t.Errorf("shard[0] = %+v, want tel:+15551230000 with 2 messages", split.Shards[0])
	}
	if split.Shards[1].PortalID != "mailto:sam@example.com" || split.Shards[1].Messages != 1 {
		t.Errorf("shard[1] = %+v, want mailto:sam@example.com with 1 live message", split.Shards[1])
	}
	if split.Shards[0].MXID != "!room-tel:+15551230000:hs" {
		t.Errorf("shard[0].MXID = %q, want the portal's room", split.Shards[0].MXID)
	}
}

// TestFilterUnbridgedPortals pins the filtered-chat rule, which is per-PORTAL
// and not per-row.
//
// Participant-set keying can collapse two distinct iMessage chats onto one
// portal_id, one iCloud-filtered and one not; such a portal still bridges, so
// dropping rows individually would wrongly hide it. Only a portal whose every
// live row is filtered is excluded.
func TestFilterUnbridgedPortals(t *testing.T) {
	rows := []splitConversationRow{
		{GroupID: "g", PortalID: "p-clean", Filtered: false},
		{GroupID: "g", PortalID: "p-junk", Filtered: true},
		// Mixed: one filtered chat and one not, sharing a portal. Bridges.
		{GroupID: "g", PortalID: "p-mixed", Filtered: true},
		{GroupID: "g", PortalID: "p-mixed", Filtered: false},
	}

	kept, excluded := filterUnbridgedPortals(rows, false)
	if excluded != 1 {
		t.Errorf("excluded = %d, want 1 (only p-junk)", excluded)
	}
	seen := map[string]bool{}
	for _, row := range kept {
		seen[row.PortalID] = true
	}
	if seen["p-junk"] {
		t.Error("p-junk survived; a portal whose every row is filtered is never bridged")
	}
	if !seen["p-clean"] || !seen["p-mixed"] {
		t.Errorf("kept = %v, want p-clean and p-mixed", seen)
	}

	// With the option on, is_filtered is ignored entirely.
	kept, excluded = filterUnbridgedPortals(rows, true)
	if excluded != 0 || len(kept) != len(rows) {
		t.Errorf("bridgeFilteredChats=true: kept %d/%d rows, excluded %d; want all kept, none excluded",
			len(kept), len(rows), excluded)
	}
}

// TestFormatSplitConversationsNotesFilteredExclusions makes sure a count that
// looks lower than expected carries its reason, in both the all-clear and the
// populated report.
func TestFormatSplitConversationsNotesFilteredExclusions(t *testing.T) {
	clean := formatSplitConversations(splitConversationReport{FilteredPortalsExcluded: 3})
	if !strings.Contains(clean, "3 portal(s) were excluded") || !strings.Contains(clean, "bridge_filtered_chats") {
		t.Errorf("all-clear report should explain the exclusions:\n%s", clean)
	}

	withSplits := formatSplitConversations(splitConversationReport{
		Splits: []splitConversation{{GroupID: "g", Shards: []splitConversationShard{
			{PortalID: "tel:+1", Messages: 2}, {PortalID: "tel:+2", Messages: 1},
		}}},
		FilteredPortalsExcluded: 2,
	})
	if !strings.Contains(withSplits, "2 portal(s) were excluded") {
		t.Errorf("populated report should explain the exclusions:\n%s", withSplits)
	}

	none := formatSplitConversations(splitConversationReport{})
	if strings.Contains(none, "were excluded") {
		t.Errorf("no exclusions should mean no note:\n%s", none)
	}
}
