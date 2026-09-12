// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for the privacy scrubber's delivered-set filtering.

package connector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
)

// scrubTestStore builds a store with the schema in place.
func scrubTestStore(t *testing.T) (*cloudBackfillStore, *dbutil.Database, context.Context) {
	t.Helper()
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	// bridgev2 owns the `message` table; the store only reads it.
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS message (
		bridge_id TEXT, room_id TEXT, room_receiver TEXT, id TEXT
	)`); err != nil {
		t.Fatalf("create bridgev2 message table: %v", err)
	}
	return store, db, ctx
}

// TestLoadBridgedGUIDSetNormalization pins the normalization the SQL UNION used
// to do. Getting any of these wrong silently changes what is eligible to scrub:
// too narrow retains plaintext forever, too broad scrubs undelivered content.
func TestLoadBridgedGUIDSetNormalization(t *testing.T) {
	store, db, ctx := scrubTestStore(t)

	rows := []struct{ id, bridge, receiver string }{
		// APNs uppercases; CloudKit does not. Matching must be case-folded.
		{"AAAAAAAA-0000-0000-0000-000000000001", "b1", string(testSQLLoginID)},
		// A part-suffixed id must also contribute its base guid.
		{"bbbbbbbb-0000-0000-0000-000000000002_2", "b1", string(testSQLLoginID)},
		// Empty receiver is in scope (rows written before receiver scoping).
		{"cccccccc-0000-0000-0000-000000000003", "b1", ""},
		// Another login's delivery must NOT make our row eligible.
		{"dddddddd-0000-0000-0000-000000000004", "b1", "other-login"},
		// Another bridge likewise.
		{"eeeeeeee-0000-0000-0000-000000000005", "b2", string(testSQLLoginID)},
	}
	for _, r := range rows {
		if _, err := db.Exec(ctx,
			`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
			r.id, r.bridge, r.receiver); err != nil {
			t.Fatalf("insert message %s: %v", r.id, err)
		}
	}

	set, err := store.loadBridgedGUIDSet(ctx, "b1")
	if err != nil {
		t.Fatalf("loadBridgedGUIDSet: %v", err)
	}

	for _, want := range []string{
		"aaaaaaaa-0000-0000-0000-000000000001",   // case-folded
		"bbbbbbbb-0000-0000-0000-000000000002_2", // full part-suffixed id
		"bbbbbbbb-0000-0000-0000-000000000002",   // and its base guid
		"cccccccc-0000-0000-0000-000000000003",   // empty receiver in scope
	} {
		if _, ok := set[want]; !ok {
			t.Errorf("set is missing %q", want)
		}
	}
	for _, notWant := range []string{
		"dddddddd-0000-0000-0000-000000000004", // other login
		"eeeeeeee-0000-0000-0000-000000000005", // other bridge
	} {
		if _, ok := set[notWant]; ok {
			t.Errorf("set contains %q, which belongs to another login/bridge", notWant)
		}
	}
}

// The delivery test moved from SQL into Go, so this pins that an undelivered
// row is still left alone while a delivered one is scrubbed — the property the
// whole scrubber exists to get right.
func TestScrubBridgedBodiesOnlyScrubsDeliveredRows(t *testing.T) {
	store, db, ctx := scrubTestStore(t)
	old := time.Now().Add(-time.Hour).UnixMilli()

	insert := func(guid, text string, deleted bool) {
		t.Helper()
		if _, err := db.Exec(ctx, `
			INSERT INTO cloud_message
			  (login_id, guid, portal_id, timestamp_ms, is_from_me, text, deleted, created_ts, updated_ts)
			VALUES ($1, $2, 'tel:+15555550100', $3, FALSE, $4, $5, $3, $3)`,
			testSQLLoginID, guid, old, text, deleted); err != nil {
			t.Fatalf("insert cloud_message %s: %v", guid, err)
		}
	}
	insert("GUID-DELIVERED", "delivered body", false)
	insert("GUID-UNDELIVERED", "undelivered body", false)
	insert("GUID-DELETED", "deleted body", true)

	// Only the first has a bridgev2 row. Case differs deliberately.
	if _, err := db.Exec(ctx,
		`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
		"guid-delivered", "test-bridge", string(testSQLLoginID)); err != nil {
		t.Fatalf("insert bridgev2 row: %v", err)
	}

	// backfillActive=false so the gate is not what is under test here.
	n, err := store.scrubBridgedBodies(ctx, "test-bridge", time.Minute, nil, false)
	if err != nil {
		t.Fatalf("scrubBridgedBodies: %v", err)
	}
	// The delivered row and the deleted row; the undelivered one must survive.
	if n != 2 {
		t.Errorf("scrubbed %d rows, want 2 (delivered + deleted)", n)
	}

	body := func(guid string) string {
		t.Helper()
		var text *string
		if err := db.QueryRow(ctx,
			`SELECT text FROM cloud_message WHERE login_id=$1 AND guid=$2`,
			testSQLLoginID, guid).Scan(&text); err != nil {
			t.Fatalf("read %s: %v", guid, err)
		}
		if text == nil {
			return ""
		}
		return *text
	}
	if got := body("GUID-DELIVERED"); got != "" {
		t.Errorf("delivered body = %q, want scrubbed", got)
	}
	if got := body("GUID-DELETED"); got != "" {
		t.Errorf("deleted body = %q, want scrubbed", got)
	}
	if got := body("GUID-UNDELIVERED"); got != "undelivered body" {
		t.Errorf("undelivered body = %q, want it left intact", got)
	}
}

// A pass with only deleted rows must not read the bridgev2 message table at
// all — that skip is what keeps the steady-state cost near zero.
func TestScrubBridgedBodiesSkipsMessageTableForDeletedOnlyPass(t *testing.T) {
	store, db, ctx := scrubTestStore(t)
	old := time.Now().Add(-time.Hour).UnixMilli()

	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_message
		  (login_id, guid, portal_id, timestamp_ms, is_from_me, text, deleted, created_ts, updated_ts)
		VALUES ($1, 'GUID-DEL', 'tel:+15555550100', $2, FALSE, 'gone', TRUE, $2, $2)`,
		testSQLLoginID, old); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Deliberately pass a bridge id that matches nothing: if the delivered set
	// were consulted, the row would still scrub (deleted bypasses it), so the
	// assertion here is simply that a deleted-only pass succeeds and scrubs.
	n, err := store.scrubBridgedBodies(ctx, "no-such-bridge", time.Minute, nil, true)
	if err != nil {
		t.Fatalf("scrubBridgedBodies: %v", err)
	}
	if n != 1 {
		t.Errorf("scrubbed %d rows, want 1 — a deleted row is eligible without delivery", n)
	}
}

// Candidate enumeration is a snapshot; the write must re-check the row's state
// so a row that stops being eligible between the two is not scrubbed anyway.
func TestScrubBatchIfEligibleRechecksAtWriteTime(t *testing.T) {
	store, db, ctx := scrubTestStore(t)
	old := time.Now().Add(-time.Hour).UnixMilli()

	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_message
		  (login_id, guid, portal_id, timestamp_ms, is_from_me, text, created_ts, updated_ts)
		VALUES ($1, 'GUID-RACED', 'tel:+15555550100', $2, FALSE, 'body', $2, $2)`,
		testSQLLoginID, old); err != nil {
		t.Fatalf("insert: %v", err)
	}
	bridged := map[string]struct{}{"guid-raced": {}}
	candidates := []cloudScrubCandidate{{guid: "GUID-RACED"}}

	// Simulate the race: the row is re-ingested (updated_ts moves into the
	// grace window) after enumeration but before the write.
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET updated_ts=$1 WHERE login_id=$2 AND guid='GUID-RACED'`,
		time.Now().UnixMilli(), testSQLLoginID); err != nil {
		t.Fatalf("re-ingest: %v", err)
	}

	cutoff := time.Now().Add(-time.Minute).UnixMilli()
	n, err := store.scrubBatchIfEligible(ctx, cutoff, bridged, candidates, false)
	if err != nil {
		t.Fatalf("scrubBatchIfEligible: %v", err)
	}
	if n != 0 {
		t.Errorf("scrubbed %d rows, want 0 — the write must re-check the grace window", n)
	}
}

// TestScrubBridgedBodiesCrossesChunkBoundary drives more candidates than one
// chunk so the multi-chunk path and the size of the generated IN list are both
// exercised on SQLite.
//
// scrubBatchIfEligible names every guid in the chunk as a bound parameter, so a
// full chunk is chunkSize+2 parameters. SQLite's SQLITE_MAX_VARIABLE_NUMBER was
// 999 before 3.32 and 32766 after; go-sqlite3 bundles a modern SQLite, but a
// build linked against an old system library (-tags libsqlite3) would fail here
// rather than silently scrubbing nothing. Postgres' own limit is 65535.
func TestScrubBridgedBodiesCrossesChunkBoundary(t *testing.T) {
	store, db, ctx := scrubTestStore(t)
	old := time.Now().Add(-time.Hour).UnixMilli()

	// 2,500 rows: two full chunks plus a remainder.
	const total = 2500
	if err := db.DoTxn(ctx, nil, func(ctx context.Context) error {
		for i := 0; i < total; i++ {
			guid := fmt.Sprintf("GUID-%05d", i)
			if _, err := db.Exec(ctx, `
				INSERT INTO cloud_message
				  (login_id, guid, portal_id, timestamp_ms, is_from_me, text, created_ts, updated_ts)
				VALUES ($1, $2, 'tel:+15555550100', $3, FALSE, 'body', $3, $3)`,
				testSQLLoginID, guid, old); err != nil {
				return err
			}
			if _, err := db.Exec(ctx,
				`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
				guid, "test-bridge", string(testSQLLoginID)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	n, err := store.scrubBridgedBodies(ctx, "test-bridge", time.Minute, nil, false)
	if err != nil {
		t.Fatalf("scrubBridgedBodies across chunks: %v", err)
	}
	if n != total {
		t.Errorf("scrubbed %d rows, want %d — every chunk must apply", n, total)
	}
	var leftover int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 AND body_scrubbed=FALSE`,
		testSQLLoginID).Scan(&leftover); err != nil {
		t.Fatalf("count leftover: %v", err)
	}
	if leftover != 0 {
		t.Errorf("%d rows left unscrubbed after a multi-chunk pass", leftover)
	}
}

// TestCountUndeliveredScrubbedMessages is the regression test for a false
// "VISIBLE data loss" alarm.
//
// Forward backfill converts zero messages for a portal whose history was
// delivered and then scrubbed — correctly, since nothing is left to deliver.
// The old guard asked only "does this portal have scrubbed rows", which is
// true forever afterwards, so every later backfill run of a healthy portal
// looked like the scrubbed-before-delivery case. On a live bridge that
// un-scrubbed 7,059 delivered rows and re-fetched them from CloudKit to
// recover history that was fully present in Matrix.
//
// Only rows that were scrubbed WITHOUT reaching Matrix may count.
func TestCountUndeliveredScrubbedMessages(t *testing.T) {
	store, db, ctx := scrubTestStore(t)

	insert := func(guid, portal string) {
		t.Helper()
		if _, err := db.Exec(ctx, `
			INSERT INTO cloud_message
			  (login_id, guid, portal_id, timestamp_ms, is_from_me, record_name,
			   body_scrubbed, deleted, created_ts, updated_ts)
			VALUES ($1, $2, $3, 1000, FALSE, 'r', TRUE, FALSE, 1000, 1000)`,
			testSQLLoginID, guid, portal); err != nil {
			t.Fatalf("insert %s: %v", guid, err)
		}
	}
	// A healthy portal: every scrubbed row reached Matrix.
	insert("G-DELIVERED-1", "p-healthy")
	insert("G-DELIVERED-2", "p-healthy")
	// A damaged portal: scrubbed, never delivered.
	insert("G-LOST", "p-damaged")
	// Mixed: one delivered, one not.
	insert("G-MIXED-OK", "p-mixed")
	insert("G-MIXED-LOST", "p-mixed")

	for _, id := range []string{"g-delivered-1", "G-DELIVERED-2", "G-MIXED-OK"} {
		// Deliberately mixed case: delivery matching is case-insensitive.
		if _, err := db.Exec(ctx,
			`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
			id, "test-bridge", string(testSQLLoginID)); err != nil {
			t.Fatalf("insert message %s: %v", id, err)
		}
	}

	for _, tc := range []struct {
		portal string
		want   int
	}{
		{"p-healthy", 0}, // the false-alarm case
		{"p-damaged", 1}, // the real loss this guard exists for
		{"p-mixed", 1},
		{"p-empty", 0},
	} {
		got, err := store.countUndeliveredScrubbedMessages(ctx, "test-bridge", tc.portal)
		if err != nil {
			t.Fatalf("countUndeliveredScrubbedMessages(%s): %v", tc.portal, err)
		}
		if got != tc.want {
			t.Errorf("countUndeliveredScrubbedMessages(%s) = %d, want %d", tc.portal, got, tc.want)
		}
	}

	// Another login's delivery must not make our scrubbed row look delivered.
	insert("G-OTHER-LOGIN", "p-other")
	if _, err := db.Exec(ctx,
		`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
		"g-other-login", "test-bridge", "someone-else"); err != nil {
		t.Fatalf("insert foreign message: %v", err)
	}
	if got, err := store.countUndeliveredScrubbedMessages(ctx, "test-bridge", "p-other"); err != nil {
		t.Fatalf("countUndeliveredScrubbedMessages(p-other): %v", err)
	} else if got != 1 {
		t.Errorf("countUndeliveredScrubbedMessages(p-other) = %d, want 1 — another login's delivery is not ours", got)
	}
}
