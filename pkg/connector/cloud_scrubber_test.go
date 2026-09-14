// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for the privacy scrubber's delivered-set filtering.

package connector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
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

// TestCachedBridgedGUIDSetReusesOneLoad pins the memoisation that keeps the
// per-portal delivery check from re-reading the whole message table.
//
// loadBridgedGUIDSet reads every delivered id for the login, so calling it once
// per portal reintroduces the cost the scrubber restructure removed. Group
// consolidation resets fwd_backfill_done for every group, sending a wave of
// delivered-and-scrubbed portals down the zero-message path at once, so this is
// not a rare path.
func TestCachedBridgedGUIDSetReusesOneLoad(t *testing.T) {
	store, db, ctx := scrubTestStore(t)

	if _, err := db.Exec(ctx,
		`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
		"G-ONE", "test-bridge", string(testSQLLoginID)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	first, err := store.cachedBridgedGUIDSet(ctx, "test-bridge")
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if _, ok := first["g-one"]; !ok {
		t.Fatalf("first load missing g-one: %v", first)
	}

	// A row added after the load must NOT appear while the entry is warm —
	// that is the observable signature of reuse rather than a re-read.
	if _, err := db.Exec(ctx,
		`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
		"G-TWO", "test-bridge", string(testSQLLoginID)); err != nil {
		t.Fatalf("insert second: %v", err)
	}
	second, err := store.cachedBridgedGUIDSet(ctx, "test-bridge")
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if _, ok := second["g-two"]; ok {
		t.Error("cachedBridgedGUIDSet re-read the table; the wave would pay a full scan per portal")
	}

	// The assertion above is also the safety property: a row delivered after
	// the load is absent from the cached set, so it reads as NOT delivered.
	// That is the conservative direction — it can over-report loss and take the
	// recovery path, never under-report and mark a portal done on a stale view.

	// A different bridge id must not be served from the cache.
	other, err := store.cachedBridgedGUIDSet(ctx, "other-bridge")
	if err != nil {
		t.Fatalf("other bridge: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("cachedBridgedGUIDSet(other-bridge) = %v, want empty — the cache is per bridge id", other)
	}
}

// TestCachedBridgedGUIDSetExpiresAndReleases drives the TTL through the
// store's injected clock, so both expiry and the release are covered without
// sleeping for the real two minutes.
func TestCachedBridgedGUIDSetExpiresAndReleases(t *testing.T) {
	store, db, ctx := scrubTestStore(t)
	clock := time.Now()
	store.now = func() time.Time { return clock }

	if _, err := db.Exec(ctx,
		`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
		"G-ONE", "test-bridge", string(testSQLLoginID)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := store.cachedBridgedGUIDSet(ctx, "test-bridge"); err != nil {
		t.Fatalf("first load: %v", err)
	}

	// A row delivered after the load is invisible while the entry is warm.
	if _, err := db.Exec(ctx,
		`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
		"G-TWO", "test-bridge", string(testSQLLoginID)); err != nil {
		t.Fatalf("insert second: %v", err)
	}
	warm, err := store.cachedBridgedGUIDSet(ctx, "test-bridge")
	if err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if _, ok := warm["g-two"]; ok {
		t.Fatal("warm read saw a later delivery; the cache is not being reused")
	}

	// Past the TTL, the next read must reload and pick it up.
	clock = clock.Add(bridgedSetCacheTTL + time.Second)
	fresh, err := store.cachedBridgedGUIDSet(ctx, "test-bridge")
	if err != nil {
		t.Fatalf("read after expiry: %v", err)
	}
	if _, ok := fresh["g-two"]; !ok {
		t.Error("read after expiry did not reload; the set would stay stale forever")
	}

	// releaseExpiredBridgedGUIDSet must keep a warm entry and drop an expired
	// one, so a quiet bridge doesn't retain the set for the whole process.
	store.releaseExpiredBridgedGUIDSet()
	if store.bridgedSet == nil {
		t.Error("release dropped a warm entry; the next wave would reload for nothing")
	}
	clock = clock.Add(bridgedSetCacheTTL + time.Second)
	store.releaseExpiredBridgedGUIDSet()
	if store.bridgedSet != nil {
		t.Error("release left an expired entry referenced; ~30MB would be held for the life of the process")
	}
	// Releasing with nothing cached must be a no-op rather than a panic.
	store.releaseExpiredBridgedGUIDSet()
}

// TestScrubBridgedBodiesPagesPastUndeliveredPrefix is the regression test for
// the keyset cursor.
//
// A plain LIMIT would re-read the same prefix on every pass: rows a pass
// declines to scrub (not yet delivered) stay in the candidate set and keep
// filling that prefix, so anything behind them never gets reached. Here the
// oldest 1,200 rows — more than one page — are undelivered, and the delivered
// rows sit behind them. All of the delivered ones must still be scrubbed in a
// single pass.
func TestScrubBridgedBodiesPagesPastUndeliveredPrefix(t *testing.T) {
	store, db, ctx := scrubTestStore(t)
	base := time.Now().Add(-time.Hour).UnixMilli()

	const undeliveredCount = 1200
	const deliveredCount = 300
	if err := db.DoTxn(ctx, nil, func(ctx context.Context) error {
		for i := 0; i < undeliveredCount+deliveredCount; i++ {
			guid := fmt.Sprintf("GUID-%05d", i)
			// updated_ts ascending, so the undelivered rows are the oldest and
			// form the prefix the cursor has to step past.
			if _, err := db.Exec(ctx, `
				INSERT INTO cloud_message
				  (login_id, guid, portal_id, timestamp_ms, is_from_me, text, created_ts, updated_ts)
				VALUES ($1, $2, 'tel:+15555550100', $3, FALSE, 'body', $3, $3)`,
				testSQLLoginID, guid, base+int64(i)); err != nil {
				return err
			}
			if i >= undeliveredCount {
				if _, err := db.Exec(ctx,
					`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
					guid, "test-bridge", string(testSQLLoginID)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := store.scrubBridgedBodies(ctx, "test-bridge", time.Minute, nil, false)
	if err != nil {
		t.Fatalf("scrubBridgedBodies: %v", err)
	}
	if n != deliveredCount {
		t.Errorf("scrubbed %d rows, want %d — the cursor must step past the undelivered prefix", n, deliveredCount)
	}

	// And the undelivered prefix must be untouched.
	var leftover int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 AND body_scrubbed=FALSE`,
		testSQLLoginID).Scan(&leftover); err != nil {
		t.Fatalf("count: %v", err)
	}
	if leftover != undeliveredCount {
		t.Errorf("%d rows left unscrubbed, want %d (the undelivered prefix only)", leftover, undeliveredCount)
	}
}

// TestUndeliveredScrubbedInWindowIgnoresUnreachableTail is the capped-install
// regression test.
//
// With backfill.max_initial_messages capped, scrubUnbridgedTail clears rows
// older than the newest N without a delivery check, because backfill can never
// reach them. Those rows are therefore scrubbed AND undelivered by design. A
// portal-wide delivery check counted them as loss, so every capped portal that
// converted to zero messages got its whole history un-scrubbed and re-fetched
// from CloudKit — which the tail scrubber then cleared again on a later tick.
//
// Judging only the conversion window excludes them structurally: the tail
// threshold is computed with listLatestMessages' own predicate and ordering,
// and the window is what that call returned.
func TestUndeliveredScrubbedInWindowIgnoresUnreachableTail(t *testing.T) {
	store, db, ctx := scrubTestStore(t)
	c := &IMClient{cloudStore: store}

	// Stand in for what listLatestMessages returned: the newest rows only.
	// All are scrubbed and all were delivered, so nothing is lost.
	window := []cloudMessageRow{
		{GUID: "G-NEW-1", BodyScrubbed: true},
		{GUID: "G-NEW-2", BodyScrubbed: true},
	}
	for _, id := range []string{"g-new-1", "G-NEW-2"} {
		if _, err := db.Exec(ctx,
			`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
			id, "", string(testSQLLoginID)); err != nil {
			t.Fatalf("insert message %s: %v", id, err)
		}
	}
	// The unreachable tail: scrubbed, never delivered, and deliberately NOT in
	// the window. A portal-wide check would have counted these.
	for i := 0; i < 500; i++ {
		if _, err := db.Exec(ctx, `
			INSERT INTO cloud_message
			  (login_id, guid, portal_id, timestamp_ms, is_from_me, record_name,
			   body_scrubbed, deleted, created_ts, updated_ts)
			VALUES ($1, $2, 'p-capped', $3, FALSE, 'r', TRUE, FALSE, $3, $3)`,
			testSQLLoginID, fmt.Sprintf("G-TAIL-%03d", i), 1000+int64(i)); err != nil {
			t.Fatalf("insert tail row: %v", err)
		}
	}

	log := zerolog.Nop()
	undelivered, failed := c.undeliveredScrubbedInWindow(ctx, &log, "", "p-capped", window)
	if failed {
		t.Fatal("delivery check reported failure on a healthy database")
	}
	if len(undelivered) != 0 {
		t.Errorf("undelivered = %v, want none — the unreachable tail is not loss", undelivered)
	}

	// A genuinely lost row INSIDE the window must still be caught, or the
	// rescoping would have removed the guard's reason to exist.
	window = append(window, cloudMessageRow{GUID: "G-LOST", BodyScrubbed: true})
	undelivered, failed = c.undeliveredScrubbedInWindow(ctx, &log, "", "p-capped", window)
	if failed {
		t.Fatal("unexpected delivery-check failure")
	}
	if len(undelivered) != 1 || undelivered[0] != "G-LOST" {
		t.Errorf("undelivered = %v, want exactly [G-LOST]", undelivered)
	}
}

// Rows the window carries that are not scrubbed, or are reactions, are not loss
// and must not drag the portal into recovery.
func TestUndeliveredScrubbedInWindowSkipsUnscrubbedAndReactions(t *testing.T) {
	store, _, ctx := scrubTestStore(t)
	c := &IMClient{cloudStore: store}
	reaction := uint32(2000)

	window := []cloudMessageRow{
		{GUID: "G-LIVE"}, // not scrubbed
		{GUID: "G-REACTION", BodyScrubbed: true, TapbackType: &reaction}, // scrubbed reaction
	}
	log := zerolog.Nop()
	undelivered, failed := c.undeliveredScrubbedInWindow(ctx, &log, "", "p", window)
	if failed {
		t.Fatal("unexpected delivery-check failure")
	}
	if len(undelivered) != 0 {
		t.Errorf("undelivered = %v, want none", undelivered)
	}
}
