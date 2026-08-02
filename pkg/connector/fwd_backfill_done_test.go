package connector

import (
	"context"
	"database/sql"
	"testing"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// openTestCloudChatDB builds a minimal in-memory SQLite cloud_chat table for
// exercising the fwd_backfill_done store methods. SQLite's dynamic typing
// means it won't catch the Postgres "boolean = integer" class of bug on its
// own (verified separately against a live Postgres DB in a rolled-back
// transaction) — this test guards the surrounding logic instead: markDone's
// self-healing INSERT, isDone's read, and reset's clear.
func openTestCloudChatDB(t *testing.T) *cloudBackfillStore {
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

	schema := `CREATE TABLE cloud_chat (
		login_id TEXT, cloud_chat_id TEXT, portal_id TEXT, group_id TEXT,
		display_name TEXT, group_photo_guid TEXT, participants_json TEXT,
		created_ts INTEGER, updated_ts INTEGER, deleted BOOLEAN NOT NULL DEFAULT FALSE,
		fwd_backfill_done BOOLEAN NOT NULL DEFAULT FALSE,
		PRIMARY KEY (login_id, cloud_chat_id)
	)`
	if _, err := rawDB.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return newCloudBackfillStore(db, networkid.UserLoginID("login1"))
}

func TestForwardBackfillDoneLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openTestCloudChatDB(t)
	const portalID = "p1"

	if store.isForwardBackfillDone(ctx, portalID) {
		t.Fatalf("isForwardBackfillDone = true before any cloud_chat row exists, want false")
	}

	// markForwardBackfillDone self-heals via a synthetic INSERT when no
	// cloud_chat row matches the portal yet (APNs-created portal case).
	store.markForwardBackfillDone(ctx, portalID)
	if !store.isForwardBackfillDone(ctx, portalID) {
		t.Fatalf("isForwardBackfillDone = false after markForwardBackfillDone (synthetic insert), want true")
	}

	donePortals, err := store.getForwardBackfillDonePortals(ctx)
	if err != nil {
		t.Fatalf("getForwardBackfillDonePortals: %v", err)
	}
	if !donePortals[portalID] {
		t.Errorf("getForwardBackfillDonePortals = %v, want %q present", donePortals, portalID)
	}

	if err := store.resetForwardBackfillDone(ctx, portalID); err != nil {
		t.Fatalf("resetForwardBackfillDone: %v", err)
	}
	if store.isForwardBackfillDone(ctx, portalID) {
		t.Fatalf("isForwardBackfillDone = true after resetForwardBackfillDone, want false")
	}

	// markForwardBackfillDone should also work via the UPDATE path once a
	// real cloud_chat row exists (not just the synthetic-insert fallback).
	store.markForwardBackfillDone(ctx, portalID)
	if !store.isForwardBackfillDone(ctx, portalID) {
		t.Fatalf("isForwardBackfillDone = false after second markForwardBackfillDone (update path), want true")
	}
}

// A failing query must report false rather than true: treating an error as
// "done" would skip backfill for the portal entirely. It must also not panic
// or block. The warning it now logs is what makes the failure visible — see
// issue #4 — but the contract tested here is the return value.
func TestIsForwardBackfillDoneReturnsFalseOnQueryError(t *testing.T) {
	store := openTestCloudChatDB(t)
	ctx := context.Background()

	// Drop the table out from under it to force a query error.
	if _, err := store.db.Exec(ctx, `DROP TABLE cloud_chat`); err != nil {
		t.Fatalf("drop cloud_chat: %v", err)
	}

	if store.isForwardBackfillDone(ctx, "p1") {
		t.Fatal("isForwardBackfillDone = true when the query failed; a failed query must never report done")
	}
}
