// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for normalizeGroupMessagePortalIDs, which maps legacy gid:<chat_id>
// message rows onto the canonical portal_id recorded in cloud_chat.

package connector

import (
	"context"
	"testing"

	"go.mau.fi/util/dbutil"
)

func gidTestStore(t *testing.T) (*cloudBackfillStore, *dbutil.Database, context.Context) {
	t.Helper()
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	return store, db, ctx
}

func gidSeedChat(t *testing.T, db *dbutil.Database, ctx context.Context, chatID, groupID, portalID string) {
	t.Helper()
	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_chat (login_id, cloud_chat_id, group_id, portal_id, created_ts)
		VALUES ($1, $2, $3, $4, 1000)`,
		testSQLLoginID, chatID, groupID, portalID); err != nil {
		t.Fatalf("seed cloud_chat %s: %v", chatID, err)
	}
}

func gidSeedMessage(t *testing.T, db *dbutil.Database, ctx context.Context, guid, portalID string) {
	t.Helper()
	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_message
		  (login_id, guid, portal_id, timestamp_ms, is_from_me, created_ts, updated_ts)
		VALUES ($1, $2, $3, 1000, FALSE, 1000, 1000)`,
		testSQLLoginID, guid, portalID); err != nil {
		t.Fatalf("seed cloud_message %s: %v", guid, err)
	}
}

func gidPortalOf(t *testing.T, db *dbutil.Database, ctx context.Context, guid string) string {
	t.Helper()
	var portalID string
	if err := db.QueryRow(ctx,
		`SELECT portal_id FROM cloud_message WHERE login_id=$1 AND guid=$2`,
		testSQLLoginID, guid).Scan(&portalID); err != nil {
		t.Fatalf("read portal_id for %s: %v", guid, err)
	}
	return portalID
}

// The core migration: a row keyed by the CloudKit chat_id UUID moves to the
// canonical portal_id, whether the UUID matches the chat's group_id or its
// cloud_chat_id, and regardless of case.
func TestNormalizeGroupMessagePortalIDsRewritesLegacyKeys(t *testing.T) {
	store, db, ctx := gidTestStore(t)

	// Matched via cloud_chat_id, and the stored portal_id is a participant key.
	gidSeedChat(t, db, ctx, "CHAT-AAA", "GROUP-AAA", "tel:+15555550101,tel:+15555550102")
	gidSeedMessage(t, db, ctx, "G-BY-CHATID", "gid:chat-aaa")
	// Matched via group_id, with the UUID in a different case.
	gidSeedChat(t, db, ctx, "CHAT-BBB", "GROUP-BBB", "gid:canonical-bbb")
	gidSeedMessage(t, db, ctx, "G-BY-GROUPID", "gid:GROUP-BBB")
	// Already canonical — must not be touched or counted.
	gidSeedChat(t, db, ctx, "CHAT-CCC", "GROUP-CCC", "gid:group-ccc")
	gidSeedMessage(t, db, ctx, "G-ALREADY-OK", "gid:group-ccc")
	// A UUID no chat claims.
	gidSeedMessage(t, db, ctx, "G-UNKNOWN", "gid:orphan-uuid")
	// A non-gid portal is out of scope entirely.
	gidSeedMessage(t, db, ctx, "G-DM", "tel:+15555550199")

	n, err := store.normalizeGroupMessagePortalIDs(ctx)
	if err != nil {
		t.Fatalf("normalizeGroupMessagePortalIDs: %v", err)
	}
	if n != 2 {
		t.Errorf("normalized %d rows, want 2", n)
	}

	for _, tc := range []struct{ guid, want string }{
		{"G-BY-CHATID", "tel:+15555550101,tel:+15555550102"},
		{"G-BY-GROUPID", "gid:canonical-bbb"},
		{"G-ALREADY-OK", "gid:group-ccc"},
		{"G-UNKNOWN", "gid:orphan-uuid"},
		{"G-DM", "tel:+15555550199"},
	} {
		if got := gidPortalOf(t, db, ctx, tc.guid); got != tc.want {
			t.Errorf("portal_id of %s = %q, want %q", tc.guid, got, tc.want)
		}
	}
}

// TestNormalizeGroupMessagePortalIDsDeclinesAmbiguousUUID is the behaviour
// change: when two chats claim one UUID with different portal_ids, the old SQL
// resolved it with a bare LIMIT 1 and could pick differently on the next run.
// Guessing a canonical is the pattern this tree removed from group
// consolidation twice; declining leaves the rows as the legacy data already had
// them rather than moving them somewhere possibly wrong.
func TestNormalizeGroupMessagePortalIDsDeclinesAmbiguousUUID(t *testing.T) {
	store, db, ctx := gidTestStore(t)

	// Two chats, same group_id, different portal_ids.
	gidSeedChat(t, db, ctx, "CHAT-1", "GROUP-DUP", "tel:+15555550101,tel:+15555550102")
	gidSeedChat(t, db, ctx, "CHAT-2", "GROUP-DUP", "tel:+15555550103,tel:+15555550104")
	gidSeedMessage(t, db, ctx, "G-AMBIG", "gid:group-dup")

	n, err := store.normalizeGroupMessagePortalIDs(ctx)
	if err != nil {
		t.Fatalf("normalizeGroupMessagePortalIDs: %v", err)
	}
	if n != 0 {
		t.Errorf("normalized %d rows, want 0 — an ambiguous UUID must not be guessed", n)
	}
	if got := gidPortalOf(t, db, ctx, "G-AMBIG"); got != "gid:group-dup" {
		t.Errorf("portal_id = %q, want it left at gid:group-dup", got)
	}

	// Two chats agreeing on the same portal_id is not ambiguous.
	gidSeedChat(t, db, ctx, "CHAT-3", "GROUP-SAME", "gid:agreed")
	gidSeedChat(t, db, ctx, "CHAT-4", "GROUP-SAME", "gid:agreed")
	gidSeedMessage(t, db, ctx, "G-AGREED", "gid:group-same")
	if n, err = store.normalizeGroupMessagePortalIDs(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	} else if n != 1 {
		t.Errorf("normalized %d rows, want 1 — agreeing chats are not ambiguous", n)
	}
	if got := gidPortalOf(t, db, ctx, "G-AGREED"); got != "gid:agreed" {
		t.Errorf("portal_id = %q, want gid:agreed", got)
	}
}

// Every row sharing a legacy portal_id moves, not just one, and a second pass
// is a no-op so the startup call is idempotent.
func TestNormalizeGroupMessagePortalIDsMovesAllRowsAndIsIdempotent(t *testing.T) {
	store, db, ctx := gidTestStore(t)
	gidSeedChat(t, db, ctx, "CHAT-AAA", "GROUP-AAA", "gid:canonical")
	for _, guid := range []string{"G-1", "G-2", "G-3"} {
		gidSeedMessage(t, db, ctx, guid, "gid:chat-aaa")
	}

	n, err := store.normalizeGroupMessagePortalIDs(ctx)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if n != 3 {
		t.Errorf("normalized %d rows, want 3", n)
	}
	n, err = store.normalizeGroupMessagePortalIDs(ctx)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n != 0 {
		t.Errorf("second pass normalized %d rows, want 0 — the startup call must be idempotent", n)
	}
}

func TestCanonicalPortalForGid(t *testing.T) {
	canonical := map[string]map[string]struct{}{
		"uuid-one":   {"gid:one": {}},
		"uuid-many":  {"gid:a": {}, "gid:b": {}},
		"uuid-empty": {"": {}},
	}
	for _, tc := range []struct {
		portalID string
		want     string
		wantOK   bool
	}{
		{"gid:uuid-one", "gid:one", true},
		{"gid:UUID-ONE", "gid:one", true}, // case-folded
		{"gid:uuid-many", "", false},      // ambiguous
		{"gid:uuid-empty", "", false},     // a chat with no portal is no answer
		{"gid:uuid-missing", "", false},   // unknown
		{"tel:+15555550101", "", false},   // not a gid: key at all
	} {
		got, ok := canonicalPortalForGid(canonical, tc.portalID)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("canonicalPortalForGid(%q) = (%q, %v), want (%q, %v)",
				tc.portalID, got, ok, tc.want, tc.wantOK)
		}
	}
}
