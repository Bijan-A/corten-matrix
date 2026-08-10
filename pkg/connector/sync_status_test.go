package connector

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
)

// openTestSyncDB builds an in-memory SQLite database with just enough schema
// (corten-matrix's own cloud_* tables plus the bridgev2 `user_login` and
// `message` tables this code reads) to exercise GetSyncStatus end to end,
// including its Postgres/SQLite-portable SQL (INSTR-based guid
// normalization, dialect-aware queries).
func openTestSyncDB(t *testing.T) *dbutil.Database {
	t.Helper()
	rawDB, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { rawDB.Close() })

	db, err := dbutil.NewWithDB(rawDB, "sqlite3")
	if err != nil {
		t.Fatalf("wrap db: %v", err)
	}

	schema := `
	CREATE TABLE cloud_sync_state (
		login_id TEXT, zone TEXT, continuation_token TEXT,
		last_success_ts INTEGER, last_error TEXT, updated_ts INTEGER,
		PRIMARY KEY (login_id, zone)
	);
	CREATE TABLE cloud_chat (
		login_id TEXT, portal_id TEXT, cloud_chat_id TEXT, group_id TEXT,
		deleted BOOLEAN, is_filtered INTEGER, updated_ts INTEGER,
		participants_json TEXT
	);
	CREATE TABLE cloud_message (
		login_id TEXT, guid TEXT, record_name TEXT, portal_id TEXT,
		deleted BOOLEAN, tapback_type INTEGER, timestamp_ms INTEGER,
		updated_ts INTEGER, sender TEXT, is_from_me BOOLEAN DEFAULT 0,
		text TEXT, attachments_json TEXT, has_body BOOLEAN DEFAULT 1,
		body_scrubbed BOOLEAN DEFAULT 0
	);
	CREATE TABLE portal (id TEXT, receiver TEXT, mxid TEXT);
	CREATE TABLE user_login (id TEXT, bridge_id TEXT);
	CREATE TABLE message (id TEXT, bridge_id TEXT, room_receiver TEXT);
	CREATE TABLE kv_store (bridge_id TEXT, key TEXT, value TEXT, PRIMARY KEY (bridge_id, key));
	`
	if _, err := rawDB.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return db
}

func TestGetSyncStatus(t *testing.T) {
	ctx := context.Background()
	db := openTestSyncDB(t)
	const loginID = "login1"
	const bridgeID = ""

	now := time.Now().UnixMilli()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.RawDB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	exec(`INSERT INTO user_login (id, bridge_id) VALUES (?, ?)`, loginID, bridgeID)
	exec(`INSERT INTO cloud_sync_state (login_id, zone, continuation_token, last_success_ts, last_error, updated_ts) VALUES (?, ?, ?, ?, NULL, ?)`,
		loginID, cloudZoneChats, "tok1", now, now)
	exec(`INSERT INTO cloud_sync_state (login_id, zone, continuation_token, last_success_ts, last_error, updated_ts) VALUES (?, ?, ?, ?, ?, ?)`,
		loginID, cloudZoneMessages, "tok2", now, "boom", now)
	// attachments zone never synced: no row.

	// Chats: p1 and p_noroom are bridgeable (not filtered, not deleted);
	// p_filtered is iMessage-filtered (never bridges); p_deleted is soft-deleted.
	exec(`INSERT INTO cloud_chat (login_id, portal_id, deleted, is_filtered) VALUES (?, 'p1', 0, 0)`, loginID)
	exec(`INSERT INTO cloud_chat (login_id, portal_id, deleted, is_filtered) VALUES (?, 'p_noroom', 0, 0)`, loginID)
	exec(`INSERT INTO cloud_chat (login_id, portal_id, deleted, is_filtered) VALUES (?, 'p_filtered', 0, 1)`, loginID)
	exec(`INSERT INTO cloud_chat (login_id, portal_id, deleted, is_filtered) VALUES (?, 'p_deleted', 1, 0)`, loginID)

	// Deliverability does NOT require a Matrix room: a bridgeable message in a
	// roomless-but-unfiltered chat (guid-nr) is deliverable-but-pending, which is
	// the core of this fix. Content = has_body OR text OR attachments, so a
	// delivered-then-scrubbed row (guid-s: text AND sender NULLed, but has_body
	// and body_scrubbed still set) stays deliverable and doesn't push the ratio
	// over 100%. A no-sender/not-from-me system record (guid-sys) is excluded to
	// match bridgev2's runtime skip.
	ins := func(guid, rec, portal, sender string, fromMe, deleted, tapback, hasBody int, text string, scrubbed int) {
		t.Helper()
		var tb any
		if tapback != 0 {
			tb = tapback
		}
		exec(`INSERT INTO cloud_message (login_id, guid, record_name, portal_id, sender, is_from_me, deleted, tapback_type, has_body, text, body_scrubbed, timestamp_ms, updated_ts)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, loginID, guid, rec, portal, sender, fromMe, deleted, tb, hasBody, text, scrubbed, now, now)
	}
	//    guid,      rec,     portal,        sender,    fromMe del tap  body text     scrub
	ins("guid-a", "rec-a", "p1", "tel:+1", 0, 0, 0, 1, "hi", 0)         // deliverable, delivered (direct)
	ins("guid-b", "rec-b", "p1", "tel:+1", 0, 0, 0, 1, "hi", 0)         // deliverable, delivered (part-suffix)
	ins("guid-c", "rec-c", "p1", "tel:+1", 0, 0, 0, 1, "hi", 0)         // deliverable, pending
	ins("guid-s", "rec-s", "p1", "", 0, 0, 0, 1, "", 1)                 // deliverable (has_body+scrubbed), delivered
	ins("guid-nr", "rec-nr", "p_noroom", "tel:+2", 0, 0, 0, 1, "hi", 0) // deliverable (no room yet), pending
	ins("guid-r", "rec-r", "p1", "tel:+1", 0, 0, 2000, 1, "react", 0)   // reaction — excluded
	ins("guid-d", "rec-d", "p1", "tel:+1", 0, 1, 0, 1, "hi", 0)         // deleted message — excluded
	ins("guid-e", "rec-e", "p_filtered", "tel:+3", 0, 0, 0, 1, "hi", 0) // filtered chat — unbridgeable
	ins("guid-f", "rec-f", "p1", "tel:+5", 0, 0, 0, 0, "", 0)           // empty/system (no content) — unbridgeable
	ins("guid-g", "rec-g", "p_deleted", "tel:+4", 0, 0, 0, 1, "hi", 0)  // deleted chat — unbridgeable
	ins("guid-sys", "rec-sys", "p1", "", 0, 0, 0, 1, "sysnote", 0)      // no sender, not from-me — unbridgeable

	exec(`INSERT INTO message (id, bridge_id, room_receiver) VALUES ('GUID-A', ?, ?)`, bridgeID, loginID)
	exec(`INSERT INTO message (id, bridge_id, room_receiver) VALUES ('guid-b_1', ?, ?)`, bridgeID, loginID)
	exec(`INSERT INTO message (id, bridge_id, room_receiver) VALUES ('GUID-S', ?, ?)`, bridgeID, loginID)

	report, err := GetSyncStatus(ctx, db, bridgeID, 0)
	if err != nil {
		t.Fatalf("GetSyncStatus: %v", err)
	}

	if !report.BootstrapComplete {
		t.Errorf("BootstrapComplete = false, want true")
	}
	if report.TotalChats != 3 {
		t.Errorf("TotalChats = %d, want 3 (p1, p_noroom, p_filtered; p_deleted excluded)", report.TotalChats)
	}
	if report.DeliverableMessages != 5 {
		t.Errorf("DeliverableMessages = %d, want 5 (guid-a,b,c,s,nr; excludes reaction, deleted msg, filtered chat, empty, deleted chat)", report.DeliverableMessages)
	}
	if report.DeliveredMessages != 3 {
		t.Errorf("DeliveredMessages = %d, want 3 (guid-a direct, guid-b part-suffix, guid-s scrubbed-but-delivered)", report.DeliveredMessages)
	}
	if report.UnbridgeableMessages != 4 {
		t.Errorf("UnbridgeableMessages = %d, want 4 (filtered-chat, empty, deleted-chat, no-sender system)", report.UnbridgeableMessages)
	}
	if got := report.PendingMessages(); got != 2 {
		t.Errorf("PendingMessages() = %d, want 2 (guid-c, guid-nr)", got)
	}
	if report.FullyCaughtUp() {
		t.Errorf("FullyCaughtUp() = true, want false (2 messages still pending)")
	}

	var chatsZone, messagesZone, attachmentsZone *ZoneSyncStatus
	for i := range report.Zones {
		switch report.Zones[i].Zone {
		case cloudZoneChats:
			chatsZone = &report.Zones[i]
		case cloudZoneMessages:
			messagesZone = &report.Zones[i]
		case cloudZoneAttachments:
			attachmentsZone = &report.Zones[i]
		}
	}
	if chatsZone == nil || chatsZone.LastSuccess == nil {
		t.Fatalf("chats zone missing LastSuccess")
	}
	if messagesZone == nil || messagesZone.LastError != "boom" {
		t.Fatalf("messages zone LastError = %v, want \"boom\"", messagesZone)
	}
	if attachmentsZone == nil || attachmentsZone.LastSuccess != nil {
		t.Errorf("attachments zone should have no LastSuccess (never synced)")
	}

	out := report.Format(nil)
	if out == "" {
		t.Errorf("Format() returned empty string")
	}
}

func TestSyncStatusReportFullyCaughtUp(t *testing.T) {
	tests := []struct {
		name        string
		bootstrap   bool
		deliverable int
		delivered   int
		want        bool
	}{
		{"not bootstrapped, nothing pending", false, 5, 5, false},
		{"bootstrapped, backlog remains", true, 5, 3, false},
		{"bootstrapped, fully delivered", true, 5, 5, true},
		// Zero deliverable must NOT report caught-up: otherwise the one-time
		// "sync complete — all 0 messages" notice fires early and latches.
		{"bootstrapped, nothing ever deliverable", true, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &SyncStatusReport{
				BootstrapComplete:   tt.bootstrap,
				DeliverableMessages: tt.deliverable,
				DeliveredMessages:   tt.delivered,
			}
			if got := r.FullyCaughtUp(); got != tt.want {
				t.Errorf("FullyCaughtUp() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStatusKitUpdateCounter(t *testing.T) {
	ctx := context.Background()
	db := openTestSyncDB(t)
	const bridgeID = ""

	count, lastAt, err := getStatusKitUpdateStats(ctx, db, bridgeID)
	if err != nil {
		t.Fatalf("getStatusKitUpdateStats (empty): %v", err)
	}
	if count != 0 || lastAt != nil {
		t.Fatalf("getStatusKitUpdateStats (empty) = (%d, %v), want (0, nil)", count, lastAt)
	}

	log := zerolog.Nop()
	before := time.Now()
	incrStatusKitUpdateCounter(ctx, db, bridgeID, log)
	incrStatusKitUpdateCounter(ctx, db, bridgeID, log)
	after := time.Now()

	count, lastAt, err = getStatusKitUpdateStats(ctx, db, bridgeID)
	if err != nil {
		t.Fatalf("getStatusKitUpdateStats: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	if lastAt == nil || lastAt.Before(before.Add(-time.Second)) || lastAt.After(after.Add(time.Second)) {
		t.Errorf("lastAt = %v, want between %v and %v", lastAt, before, after)
	}

	// The live-message and StatusKit counters must be independent —
	// bumping one must not affect the other.
	liveCount, _, err := getLiveMessageStats(ctx, db, bridgeID)
	if err != nil {
		t.Fatalf("getLiveMessageStats: %v", err)
	}
	if liveCount != 0 {
		t.Errorf("live message count = %d, want 0 (unaffected by StatusKit counter)", liveCount)
	}
}

func TestGetSyncStatusNoLogin(t *testing.T) {
	db := openTestSyncDB(t)
	if _, err := GetSyncStatus(context.Background(), db, "", 0); err == nil {
		t.Errorf("expected error when no user_login row exists")
	}
}

func TestLiveMessageCounter(t *testing.T) {
	ctx := context.Background()
	db := openTestSyncDB(t)
	const bridgeID = ""

	count, lastAt, err := getLiveMessageStats(ctx, db, bridgeID)
	if err != nil {
		t.Fatalf("getLiveMessageStats (empty): %v", err)
	}
	if count != 0 || lastAt != nil {
		t.Fatalf("getLiveMessageStats (empty) = (%d, %v), want (0, nil)", count, lastAt)
	}

	log := zerolog.Nop()
	before := time.Now()
	incrLiveMessageCounter(ctx, db, bridgeID, log)
	incrLiveMessageCounter(ctx, db, bridgeID, log)
	incrLiveMessageCounter(ctx, db, bridgeID, log)
	after := time.Now()

	count, lastAt, err = getLiveMessageStats(ctx, db, bridgeID)
	if err != nil {
		t.Fatalf("getLiveMessageStats: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if lastAt == nil || lastAt.Before(before.Add(-time.Second)) || lastAt.After(after.Add(time.Second)) {
		t.Errorf("lastAt = %v, want between %v and %v", lastAt, before, after)
	}
}
