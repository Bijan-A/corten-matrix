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
		updated_ts INTEGER
	);
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

	exec(`INSERT INTO cloud_chat (login_id, portal_id, deleted) VALUES (?, 'p1', 0)`, loginID)
	exec(`INSERT INTO cloud_chat (login_id, portal_id, deleted) VALUES (?, 'p2', 1)`, loginID) // deleted, excluded

	// 3 deliverable messages: 2 delivered (one matched directly, one via a
	// part-suffixed bridgev2 message id), 1 pending. Plus a reaction (should
	// be excluded from both counts) and a deleted message (excluded).
	exec(`INSERT INTO cloud_message (login_id, guid, record_name, portal_id, deleted, tapback_type, timestamp_ms, updated_ts) VALUES (?, 'guid-a', 'rec-a', 'p1', 0, NULL, ?, ?)`, loginID, now, now)
	exec(`INSERT INTO cloud_message (login_id, guid, record_name, portal_id, deleted, tapback_type, timestamp_ms, updated_ts) VALUES (?, 'guid-b', 'rec-b', 'p1', 0, NULL, ?, ?)`, loginID, now, now)
	exec(`INSERT INTO cloud_message (login_id, guid, record_name, portal_id, deleted, tapback_type, timestamp_ms, updated_ts) VALUES (?, 'guid-c', 'rec-c', 'p1', 0, NULL, ?, ?)`, loginID, now, now)
	exec(`INSERT INTO cloud_message (login_id, guid, record_name, portal_id, deleted, tapback_type, timestamp_ms, updated_ts) VALUES (?, 'guid-r', 'rec-r', 'p1', 0, 2000, ?, ?)`, loginID, now, now)
	exec(`INSERT INTO cloud_message (login_id, guid, record_name, portal_id, deleted, tapback_type, timestamp_ms, updated_ts) VALUES (?, 'guid-d', 'rec-d', 'p1', 1, NULL, ?, ?)`, loginID, now, now)

	exec(`INSERT INTO message (id, bridge_id, room_receiver) VALUES ('GUID-A', ?, ?)`, bridgeID, loginID)
	exec(`INSERT INTO message (id, bridge_id, room_receiver) VALUES ('guid-b_1', ?, ?)`, bridgeID, loginID)

	report, err := GetSyncStatus(ctx, db, bridgeID)
	if err != nil {
		t.Fatalf("GetSyncStatus: %v", err)
	}

	if !report.BootstrapComplete {
		t.Errorf("BootstrapComplete = false, want true")
	}
	if report.TotalChats != 1 {
		t.Errorf("TotalChats = %d, want 1", report.TotalChats)
	}
	if report.DeliverableMessages != 3 {
		t.Errorf("DeliverableMessages = %d, want 3 (excludes reaction + deleted)", report.DeliverableMessages)
	}
	if report.DeliveredMessages != 2 {
		t.Errorf("DeliveredMessages = %d, want 2 (direct match + part-suffixed match)", report.DeliveredMessages)
	}
	if got := report.PendingMessages(); got != 1 {
		t.Errorf("PendingMessages() = %d, want 1", got)
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

func TestGetSyncStatusNoLogin(t *testing.T) {
	db := openTestSyncDB(t)
	if _, err := GetSyncStatus(context.Background(), db, ""); err == nil {
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
