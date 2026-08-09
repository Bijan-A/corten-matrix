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

// hasScrubbedBackfillableMessages is the trigger for forward backfill's
// rehydrate-before-marking-done guard. It must fire only for deliverable rows
// whose bodies were cleared by the privacy scrubber (body_scrubbed=TRUE,
// has_body=TRUE, non-reaction) — not for genuinely-empty portals, non-scrubbed
// rows, deleted rows, or scrubbed reactions.
func TestHasScrubbedBackfillableMessages(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	const now = int64(1_000_000)

	// A live (non-scrubbed) contentful row must NOT trigger rehydration.
	if err := store.upsertMessageBatch(ctx, []cloudMessageRow{{
		GUID: "G-LIVE", PortalID: "p-live", CloudChatID: "C1",
		TimestampMS: now, Text: "hello", Service: "iMessage", HasBody: true,
	}}); err != nil {
		t.Fatalf("upsert live: %v", err)
	}
	// Give it a record_name so it counts as backfillable (matches the guard's
	// record_name <> '' predicate).
	if _, err := db.Exec(ctx, `UPDATE cloud_message SET record_name='r' WHERE login_id=$1`, testSQLLoginID); err != nil {
		t.Fatalf("set record_name: %v", err)
	}
	if got, err := store.hasScrubbedBackfillableMessages(ctx, "p-live"); err != nil {
		t.Fatalf("hasScrubbedBackfillableMessages(p-live): %v", err)
	} else if got {
		t.Errorf("hasScrubbedBackfillableMessages(p-live) = true for a live contentful row, want false")
	}

	// A scrubbed, has_body, non-reaction row on p-scrub MUST trigger.
	if err := store.upsertMessageBatch(ctx, []cloudMessageRow{{
		GUID: "G-SCRUB", PortalID: "p-scrub", CloudChatID: "C2",
		TimestampMS: now, Text: "will be scrubbed", Service: "iMessage", HasBody: true,
	}}); err != nil {
		t.Fatalf("upsert scrub: %v", err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET record_name='r', text=NULL, body_scrubbed=TRUE WHERE login_id=$1 AND guid=$2`,
		testSQLLoginID, "G-SCRUB",
	); err != nil {
		t.Fatalf("scrub G-SCRUB: %v", err)
	}
	if got, err := store.hasScrubbedBackfillableMessages(ctx, "p-scrub"); err != nil {
		t.Fatalf("hasScrubbedBackfillableMessages(p-scrub): %v", err)
	} else if !got {
		t.Errorf("hasScrubbedBackfillableMessages(p-scrub) = false for a scrubbed deliverable row, want true")
	}

	// A portal with no rows at all must NOT trigger.
	if got, err := store.hasScrubbedBackfillableMessages(ctx, "p-empty"); err != nil {
		t.Fatalf("hasScrubbedBackfillableMessages(p-empty): %v", err)
	} else if got {
		t.Errorf("hasScrubbedBackfillableMessages(p-empty) = true for an empty portal, want false")
	}

	// A scrubbed ATTACHMENT-ONLY row (has_body=FALSE) MUST trigger: conversion
	// skips every body_scrubbed non-reaction row regardless of has_body, so a
	// photo-only portal whose rows were all scrubbed would otherwise reach the
	// empty path unguarded (silent loss). The guard must not require has_body.
	if err := store.upsertMessageBatch(ctx, []cloudMessageRow{{
		GUID: "G-PHOTO", PortalID: "p-photo", CloudChatID: "C3",
		TimestampMS: now, Service: "iMessage", HasBody: false,
		AttachmentsJSON: `[{"guid":"a"}]`,
	}}); err != nil {
		t.Fatalf("upsert photo: %v", err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET record_name='r', has_body=FALSE, body_scrubbed=TRUE WHERE login_id=$1 AND guid=$2`,
		testSQLLoginID, "G-PHOTO",
	); err != nil {
		t.Fatalf("scrub G-PHOTO: %v", err)
	}
	if got, err := store.hasScrubbedBackfillableMessages(ctx, "p-photo"); err != nil {
		t.Fatalf("hasScrubbedBackfillableMessages(p-photo): %v", err)
	} else if !got {
		t.Errorf("hasScrubbedBackfillableMessages(p-photo) = false for a scrubbed attachment-only row, want true")
	}
}

// guidsWithDeliveredMessage backs 3b's reaction-flood mitigation: backfill uses
// it to skip queueing tapbacks whose target message was never delivered (which
// would otherwise flood the per-portal event loop as "target not found"). It must
// return exactly the guids that have a bridgev2 `message` row for this receiver.
func TestGuidsWithDeliveredMessage(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS message (
		id TEXT NOT NULL, bridge_id TEXT NOT NULL,
		room_receiver TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create message table: %v", err)
	}
	const bridgeID = "test-bridge"
	// DELIVERED-A: normal delivered target. DELIVERED-B: delivered under empty
	// room_receiver (appservice rows) — must still match. WRONG-LOGIN: belongs to
	// a different receiver — must NOT match.
	// PHOTO-ONLY: delivered only under a balloon-part suffix (id ==
	// "<guid>_att0"), no bare-guid text part. A tapback on it resolves via
	// resolveTapbackTargetID to the suffixed id, so it IS resolvable and must be
	// reported delivered under its BARE guid. DUP is repeated in the input to
	// exercise dedup.
	for _, r := range []struct{ id, recv string }{
		{"DELIVERED-A", string(testSQLLoginID)},
		{"DELIVERED-B", ""},
		{"WRONG-LOGIN", "someone-else"},
		{"PHOTO-ONLY_att0", string(testSQLLoginID)},
	} {
		if _, err := db.Exec(ctx,
			`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
			r.id, bridgeID, r.recv); err != nil {
			t.Fatalf("insert message %s: %v", r.id, err)
		}
	}

	got, err := store.guidsWithDeliveredMessage(ctx, bridgeID,
		[]string{"DELIVERED-A", "DELIVERED-A", "DELIVERED-B", "WRONG-LOGIN", "NEVER-DELIVERED", "PHOTO-ONLY"})
	if err != nil {
		t.Fatalf("guidsWithDeliveredMessage: %v", err)
	}
	want := map[string]bool{"DELIVERED-A": true, "DELIVERED-B": true, "PHOTO-ONLY": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for g := range want {
		if !got[g] {
			t.Errorf("guid %q missing from delivered set %v", g, got)
		}
	}
	if got["WRONG-LOGIN"] {
		t.Error("WRONG-LOGIN (different receiver) must not be reported delivered")
	}
	if got["NEVER-DELIVERED"] {
		t.Error("NEVER-DELIVERED (no message row) must not be reported delivered")
	}

	// Empty input must not error or query.
	if m, err := store.guidsWithDeliveredMessage(ctx, bridgeID, nil); err != nil || len(m) != 0 {
		t.Errorf("empty guids: got (%v, %v), want (empty, nil)", m, err)
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
