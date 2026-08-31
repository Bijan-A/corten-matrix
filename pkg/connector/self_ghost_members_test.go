// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for keeping our own handles' ghosts in room member lists.
//
// Regression context: an IsFromMe member makes bridgev2 sync the double puppet
// and return before it touches the ghost, so the ghost for that handle was never
// in the synced set and ChatMemberList.IsFull removed it with "User is not in
// remote chat". Forward backfill then re-joined it to author history. Observed on
// three consecutive startups of a live bridge: the ghost carrying the user's OWN
// name and avatar left their self-chat at 18:13:13 and rejoined at 18:14:10.

package connector

import (
	"context"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

// selfGhostTestClient builds a client with the given own handles and a
// pre-populated selfGhostRooms cache, so the member-map logic can be exercised
// without a database.
func selfGhostTestClient(primary string, aliases []string, rooms map[string]map[networkid.UserID]bool) *IMClient {
	c := &IMClient{handle: primary, allHandles: append([]string{primary}, aliases...)}
	c.selfGhostRoomsCache = rooms
	c.selfGhostRoomsLoaded = true
	return c
}

func fromMeMember(id networkid.UserID) bridgev2.ChatMember {
	return bridgev2.ChatMember{
		EventSender: bridgev2.EventSender{IsFromMe: true, Sender: id},
		Membership:  event.MembershipJoin,
	}
}

func plainMember(id networkid.UserID) bridgev2.ChatMember {
	return bridgev2.ChatMember{
		EventSender: bridgev2.EventSender{Sender: id},
		Membership:  event.MembershipJoin,
	}
}

// senders returns every EventSender.Sender in the map with its IsFromMe flag —
// what bridgev2 actually syncs (it iterates values, not keys).
func senders(memberMap map[networkid.UserID]bridgev2.ChatMember) map[networkid.UserID]bool {
	out := make(map[networkid.UserID]bool, len(memberMap))
	for _, m := range memberMap {
		// A plain entry wins over an IsFromMe one for the same sender: it is the
		// entry that gets the ghost joined.
		if existing, ok := out[m.Sender]; ok && !existing {
			continue
		}
		out[m.Sender] = m.IsFromMe
	}
	return out
}

// TestAddSelfGhostMembersKeepsPrimaryHandleGhost is the core case: a self-chat on
// the PRIMARY handle, where the ghost ID collides with the IsFromMe entry's key.
// Both identities must end up synced — the double puppet (so the user's own
// Matrix account isn't kicked when kick_matrix_users is on) and the ghost.
func TestAddSelfGhostMembersKeepsPrimaryHandleGhost(t *testing.T) {
	primary := "tel:+15551234567"
	ghostID := makeUserID(primary)
	c := selfGhostTestClient(primary, nil, map[string]map[networkid.UserID]bool{
		primary: {ghostID: true},
	})

	memberMap := map[networkid.UserID]bridgev2.ChatMember{ghostID: fromMeMember(ghostID)}
	c.addSelfGhostMembers(context.Background(), primary, memberMap)

	if len(memberMap) != 2 {
		t.Fatalf("member map has %d entries, want 2 (double puppet + own ghost): %#v", len(memberMap), memberMap)
	}
	var sawFromMe, sawGhost bool
	for key, m := range memberMap {
		if m.Sender != ghostID {
			t.Errorf("entry %q has sender %q, want %q", key, m.Sender, ghostID)
		}
		if m.IsFromMe {
			sawFromMe = true
		} else {
			sawGhost = true
			if !isSelfGhostMemberKey(key) {
				t.Errorf("plain ghost entry keyed %q, want the self-ghost suffix", key)
			}
		}
	}
	if !sawFromMe || !sawGhost {
		t.Errorf("want both an IsFromMe and a plain ghost entry, got from_me=%v ghost=%v", sawFromMe, sawGhost)
	}
}

// TestAddSelfGhostMembersKeepsSecondaryHandleGhost covers the self-chat on a
// non-primary handle (the observed case: portal mailto:adam@…, ghost
// @corten_mailto=3aadam=40…), where the DM branch deliberately omits the "other
// user" to avoid a duplicate key — leaving nothing to keep the ghost joined.
func TestAddSelfGhostMembersKeepsSecondaryHandleGhost(t *testing.T) {
	primary := "tel:+15551234567"
	secondary := "mailto:me@example.com"
	primaryGhost := makeUserID(primary)
	secondaryGhost := makeUserID(secondary)
	c := selfGhostTestClient(primary, []string{secondary}, map[string]map[networkid.UserID]bool{
		// Both own-handle ghosts authored messages in the self-chat.
		secondary: {secondaryGhost: true, primaryGhost: true},
	})

	memberMap := map[networkid.UserID]bridgev2.ChatMember{primaryGhost: fromMeMember(primaryGhost)}
	c.addSelfGhostMembers(context.Background(), secondary, memberMap)

	got := senders(memberMap)
	if fromMe, ok := got[secondaryGhost]; !ok || fromMe {
		t.Errorf("secondary handle ghost synced as (present=%v, from_me=%v), want present as a plain ghost", ok, fromMe)
	}
	if fromMe, ok := got[primaryGhost]; !ok || fromMe {
		t.Errorf("primary handle ghost synced as (present=%v, from_me=%v), want a plain ghost entry alongside the double puppet", ok, fromMe)
	}
}

// TestAddSelfGhostMembersLeavesOtherPortalsAlone: a portal where no own-handle
// ghost authored anything must not gain members — otherwise the fix would fan
// join events into thousands of rooms.
func TestAddSelfGhostMembersLeavesOtherPortalsAlone(t *testing.T) {
	primary := "tel:+15551234567"
	c := selfGhostTestClient(primary, nil, map[string]map[networkid.UserID]bool{
		"tel:+15559999999": {makeUserID(primary): true},
	})

	other := makeUserID("tel:+15550000000")
	memberMap := map[networkid.UserID]bridgev2.ChatMember{
		makeUserID(primary): fromMeMember(makeUserID(primary)),
		other:               plainMember(other),
	}
	c.addSelfGhostMembers(context.Background(), "tel:+15550000000", memberMap)

	if len(memberMap) != 2 {
		t.Errorf("member map grew to %d entries for an unaffected portal: %#v", len(memberMap), memberMap)
	}
}

// TestAddSelfGhostMembersIdempotent: repeated resyncs must not keep adding
// entries, and an own handle already synced as a plain ghost needs no duplicate.
func TestAddSelfGhostMembersIdempotent(t *testing.T) {
	primary := "tel:+15551234567"
	ghostID := makeUserID(primary)
	c := selfGhostTestClient(primary, nil, map[string]map[networkid.UserID]bool{
		"tel:+15558888888": {ghostID: true},
	})

	memberMap := map[networkid.UserID]bridgev2.ChatMember{ghostID: fromMeMember(ghostID)}
	c.addSelfGhostMembers(context.Background(), "tel:+15558888888", memberMap)
	first := len(memberMap)
	c.addSelfGhostMembers(context.Background(), "tel:+15558888888", memberMap)
	if len(memberMap) != first {
		t.Errorf("second call grew the map from %d to %d entries", first, len(memberMap))
	}

	// Already a plain ghost (e.g. a group roster entry that isn't marked
	// IsFromMe): no suffixed duplicate needed.
	plainMap := map[networkid.UserID]bridgev2.ChatMember{ghostID: plainMember(ghostID)}
	c.addSelfGhostMembers(context.Background(), "tel:+15558888888", plainMap)
	if len(plainMap) != 1 {
		t.Errorf("plain ghost entry gained a duplicate: %#v", plainMap)
	}
}

// TestOwnGhostIDs pins handle normalization and dedupe: the primary handle
// appearing in allHandles must not yield two IDs.
func TestOwnGhostIDs(t *testing.T) {
	c := &IMClient{
		handle:     "tel:+15551234567",
		allHandles: []string{"tel:+15551234567", "mailto:me@example.com", ""},
	}
	got := c.ownGhostIDs()
	if len(got) != 2 {
		t.Fatalf("ownGhostIDs() = %v, want 2 unique IDs", got)
	}
	want := map[networkid.UserID]bool{
		makeUserID("tel:+15551234567"):      true,
		makeUserID("mailto:me@example.com"): true,
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected ghost ID %q", id)
		}
	}
}

// TestMergeSelfGhostRooms pins the union of the two signals. Neither alone is
// enough: authored-messages covers ghosts that must be joined (backfill will
// re-join them to write history) but misses ones already joined without having
// written — the case that still got evicted from two shortcode DMs — and
// /joined_rooms is the reverse.
func TestMergeSelfGhostRooms(t *testing.T) {
	tel := makeUserID("tel:+15551234567")
	mail := makeUserID("mailto:me@example.com")

	authored := map[string]map[networkid.UserID]bool{
		"mailto:me@example.com": {tel: true, mail: true},
		"tel:+15559999999":      {tel: true},
	}
	joined := map[string]map[networkid.UserID]bool{
		// Same portal, ghost already known: no change.
		"tel:+15559999999": {tel: true},
		// Joined but silent — the gap this signal closes.
		"tel:+15558675309": {tel: true},
		// Another own handle joined to a portal we already track.
		"mailto:me@example.com": {mail: true},
	}

	got := mergeSelfGhostRooms(authored, joined)
	if len(got) != 3 {
		t.Fatalf("merged portals = %d, want 3: %#v", len(got), got)
	}
	if !got["tel:+15558675309"][tel] {
		t.Error("joined-but-silent portal missing from the union — the ghost would be evicted again")
	}
	if !got["mailto:me@example.com"][tel] || !got["mailto:me@example.com"][mail] {
		t.Errorf("union lost a ghost for a portal both signals mention: %#v", got["mailto:me@example.com"])
	}
	if len(got["tel:+15559999999"]) != 1 {
		t.Errorf("overlapping entry duplicated: %#v", got["tel:+15559999999"])
	}
}

// TestMergeSelfGhostRoomsEmptySides: a failed /joined_rooms call (or a bridge
// with no history yet) must degrade to the other signal, not panic or wipe.
func TestMergeSelfGhostRoomsEmptySides(t *testing.T) {
	tel := makeUserID("tel:+15551234567")
	authored := map[string]map[networkid.UserID]bool{"tel:+15559999999": {tel: true}}

	if got := mergeSelfGhostRooms(authored, nil); len(got) != 1 || !got["tel:+15559999999"][tel] {
		t.Errorf("merge with no joined-rooms data = %#v, want the authored set unchanged", got)
	}
	joined := map[string]map[networkid.UserID]bool{"tel:+15558675309": {tel: true}}
	if got := mergeSelfGhostRooms(nil, joined); len(got) != 1 || !got["tel:+15558675309"][tel] {
		t.Errorf("merge into a nil authored set = %#v, want the joined set", got)
	}
	if got := mergeSelfGhostRooms(nil, nil); len(got) != 0 {
		t.Errorf("merge of two empty sets = %#v, want empty", got)
	}
}

// TestSelfGhostRoomsQueryRunsOnSQLite is the regression test for a
// Postgres-only query shipping into a repo where SQLite is a first-class
// backend. The original used `sender_id = ANY($3)`, which SQLite rejects with
// "no such function: ANY" — and because the error path returned before marking
// the cache loaded, it re-failed once per GetChatInfo call, so on SQLite
// installs the leave/rejoin churn this file exists to stop was simply not
// fixed. Executing the real statement is the only way to catch that.
func TestSelfGhostRoomsQueryRunsOnSQLite(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	if _, err := db.Exec(ctx, `CREATE TABLE message (
		bridge_id TEXT, room_id TEXT, room_receiver TEXT, sender_id TEXT
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	rows := []struct{ bridge, room, receiver, sender string }{
		{"corten", "tel:+15550000001", "D:1", "tel:+15551234567"},      // own handle, keep
		{"corten", "tel:+15550000001", "D:1", "tel:+15559999999"},      // peer, ignore
		{"corten", "tel:+15550000002", "D:1", "mailto:me@example.com"}, // other own handle
		{"corten", "tel:+15550000003", "D:2", "tel:+15551234567"},      // another login, ignore
		{"other", "tel:+15550000004", "D:1", "tel:+15551234567"},       // another bridge, ignore
	}
	for _, r := range rows {
		if _, err := db.Exec(ctx, `INSERT INTO message (bridge_id, room_id, room_receiver, sender_id) VALUES ($1, $2, $3, $4)`,
			r.bridge, r.room, r.receiver, r.sender); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	query, args := buildSelfGhostRoomsQuery("corten", "D:1",
		[]networkid.UserID{makeUserID("tel:+15551234567"), makeUserID("mailto:me@example.com")})
	res, err := db.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("query failed on SQLite: %v", err)
	}
	defer res.Close()

	got := map[string]string{}
	for res.Next() {
		var room, sender string
		if err := res.Scan(&room, &sender); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[room] = sender
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d portals, want 2 (own handles in this bridge+login only): %#v", len(got), got)
	}
	if got["tel:+15550000001"] != "tel:+15551234567" || got["tel:+15550000002"] != "mailto:me@example.com" {
		t.Errorf("unexpected result set: %#v", got)
	}
}

// TestBuildSelfGhostRoomsQueryEmpty: an empty handle list must not emit "IN ()",
// which is a syntax error on both dialects. Unreachable through selfGhostRooms,
// which guards first, but the builder is separately callable now.
func TestBuildSelfGhostRoomsQueryEmpty(t *testing.T) {
	query, args := buildSelfGhostRoomsQuery("corten", "D:1", nil)
	if query != "" || args != nil {
		t.Errorf("buildSelfGhostRoomsQuery(nil) = (%q, %#v), want empty", query, args)
	}
}
