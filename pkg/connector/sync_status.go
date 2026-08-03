package connector

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// kv_store keys for the steady-state activity counters. Bridge-wide (not
// per-login), stored via the generic kv_store table bridgev2 already
// provides — see incrKVCounter for why this bypasses the KV.Set Go API.
const (
	liveMessageCountKVKey = "sync_status_live_message_count"
	liveMessageLastKVKey  = "sync_status_live_message_last_ts"

	statusKitUpdateCountKVKey = "sync_status_statuskit_update_count"
	statusKitUpdateLastKVKey  = "sync_status_statuskit_update_last_ts"
)

// incrKVCounter atomically bumps a persisted counter and its last-touched
// timestamp in kv_store. Used for steady-state activity counters (live
// messages, StatusKit updates) that sync-status reports alongside the
// CloudKit-sourced backfill counters, which don't move for activity that
// never goes through a CloudKit re-sync (see cloud_backfill_store.go's
// record_name filter).
//
// Uses a raw INSERT ... ON CONFLICT DO UPDATE against kv_store instead of
// KV.Get+KV.Set to avoid a read-modify-write race under concurrent callers;
// kv_store's schema (bridge_id, key, value all TEXT) is a stable,
// already-relied-upon part of the framework (see statuskit_alias_resolver.go
// for the same raw-SQL-against-kv_store pattern). Best-effort: errors are
// logged, not returned, so a KV hiccup never blocks the caller's real work.
func incrKVCounter(ctx context.Context, db *dbutil.Database, bridgeID, countKey, lastKey string, log zerolog.Logger) {
	_, err := db.Exec(ctx, `
		INSERT INTO kv_store (bridge_id, key, value) VALUES ($1, $2, '1')
		ON CONFLICT (bridge_id, key) DO UPDATE SET value = CAST(CAST(kv_store.value AS INTEGER) + 1 AS TEXT)
	`, bridgeID, countKey)
	if err != nil {
		log.Warn().Err(err).Str("counter", countKey).Msg("Failed to increment sync-status counter")
	}
	_, err = db.Exec(ctx, `
		INSERT INTO kv_store (bridge_id, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (bridge_id, key) DO UPDATE SET value = $3
	`, bridgeID, lastKey, strconv.FormatInt(time.Now().UnixMilli(), 10))
	if err != nil {
		log.Warn().Err(err).Str("counter", lastKey).Msg("Failed to update sync-status counter timestamp")
	}
}

// getKVCounterStats reads back a counter written by incrKVCounter.
func getKVCounterStats(ctx context.Context, db *dbutil.Database, bridgeID, countKey, lastKey string) (count int64, lastAt *time.Time, err error) {
	var raw string
	err = db.QueryRow(ctx, `SELECT value FROM kv_store WHERE bridge_id=$1 AND key=$2`, bridgeID, countKey).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return 0, nil, err
	}
	if raw != "" {
		count, _ = strconv.ParseInt(raw, 10, 64)
	}
	err = db.QueryRow(ctx, `SELECT value FROM kv_store WHERE bridge_id=$1 AND key=$2`, bridgeID, lastKey).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return count, nil, err
	}
	if raw != "" {
		if ms, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
			t := time.UnixMilli(ms)
			lastAt = &t
		}
	}
	return count, lastAt, nil
}

// incrLiveMessageCounter is called from handleMessage for every live APNs
// message that's handed off for delivery.
func incrLiveMessageCounter(ctx context.Context, db *dbutil.Database, bridgeID string, log zerolog.Logger) {
	incrKVCounter(ctx, db, bridgeID, liveMessageCountKVKey, liveMessageLastKVKey, log)
}

func getLiveMessageStats(ctx context.Context, db *dbutil.Database, bridgeID string) (count int64, lastAt *time.Time, err error) {
	return getKVCounterStats(ctx, db, bridgeID, liveMessageCountKVKey, liveMessageLastKVKey)
}

// incrStatusKitUpdateCounter is called wherever a StatusKit (Focus/DND)
// change queues a ChatInfoChange event. These rewrite a room's name/state
// but carry no message content, and are otherwise easy to mistake for real
// message activity when a room's "last active" sort bumps in a Matrix
// client — see the sync-status "StatusKit vs. message-bearing" breakdown.
func incrStatusKitUpdateCounter(ctx context.Context, db *dbutil.Database, bridgeID string, log zerolog.Logger) {
	incrKVCounter(ctx, db, bridgeID, statusKitUpdateCountKVKey, statusKitUpdateLastKVKey, log)
}

func getStatusKitUpdateStats(ctx context.Context, db *dbutil.Database, bridgeID string) (count int64, lastAt *time.Time, err error) {
	return getKVCounterStats(ctx, db, bridgeID, statusKitUpdateCountKVKey, statusKitUpdateLastKVKey)
}

// ZoneSyncStatus is the persisted sync state for one CloudKit zone
// (chats/messages/attachments), as recorded in cloud_sync_state.
type ZoneSyncStatus struct {
	Zone        string
	HasToken    bool
	LastSuccess *time.Time
	LastError   string
	UpdatedAt   *time.Time
}

// SyncStatusReport summarizes CloudKit ingestion and Matrix delivery
// progress for one login. Built entirely from persisted DB state, so it can
// be produced either by the running bridge process or by a standalone CLI
// invocation reading the same database directly.
type SyncStatusReport struct {
	LoginID string

	// BootstrapComplete is true once the chats and messages zones have each
	// recorded at least one successful sync — the same condition the running
	// bridge uses internally to open the APNs portal-creation gate.
	BootstrapComplete bool
	Zones             []ZoneSyncStatus

	TotalChats int

	// DeliverableMessages/DeliveredMessages cover real messages only —
	// reactions bridge into bridgev2's separate `reaction` table and are not
	// counted here.
	DeliverableMessages int
	DeliveredMessages   int

	// LiveMessageCount/LiveMessageLastAt track steady-state APNs traffic
	// handed off for live delivery to Matrix, separately from the CloudKit
	// backfill counters above (which don't move for live-only messages
	// received between CloudKit re-syncs). Cumulative since the bridge's
	// database was created — not reset per process restart.
	LiveMessageCount  int64
	LiveMessageLastAt *time.Time

	// StatusKitUpdateCount/StatusKitUpdateLastAt track ChatInfoChange events
	// from StatusKit Focus/DND changes — room name/state churn with no
	// message content. Reported alongside LiveMessageCount so "a room looks
	// recently active" can be told apart from "a room got a new message."
	StatusKitUpdateCount  int64
	StatusKitUpdateLastAt *time.Time
}

// PendingMessages returns how many deliverable messages have not yet reached
// Matrix.
func (r *SyncStatusReport) PendingMessages() int {
	n := r.DeliverableMessages - r.DeliveredMessages
	if n < 0 {
		return 0
	}
	return n
}

// FullyCaughtUp reports whether CloudKit bootstrap has completed and every
// deliverable message has reached Matrix — i.e. no backlog remains. Used
// both by Format() and by runSyncCompletionNotifyLoop's one-time
// management-room notice.
func (r *SyncStatusReport) FullyCaughtUp() bool {
	return r.BootstrapComplete && r.PendingMessages() == 0
}

// discoverLoginID finds the single user_login row for this bridge instance.
// corten-matrix's CloudKit backfill is single-account-per-process, so exactly
// one row is expected; zero means no account has ever logged in, and more
// than one is unexpected but resolved by taking the first (stable order).
func discoverLoginID(ctx context.Context, db *dbutil.Database, bridgeID string) (networkid.UserLoginID, error) {
	rows, err := db.Query(ctx, `SELECT id FROM user_login WHERE bridge_id=$1 ORDER BY id`, bridgeID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", rows.Err()
	}
	var id string
	if err := rows.Scan(&id); err != nil {
		return "", err
	}
	return networkid.UserLoginID(id), rows.Err()
}

// GetSyncStatus builds a SyncStatusReport by reading the bridge's database
// directly. It requires no running connector/client state, so it works both
// from inside the daemon (management-room `sync-status` command) and from a
// standalone CLI process that has only opened the configured database.
func GetSyncStatus(ctx context.Context, db *dbutil.Database, bridgeID string) (*SyncStatusReport, error) {
	loginID, err := discoverLoginID(ctx, db, bridgeID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up login: %w", err)
	}
	if loginID == "" {
		return nil, sql.ErrNoRows
	}

	store := newCloudBackfillStore(db, loginID)
	report := &SyncStatusReport{LoginID: string(loginID)}

	zoneDone := map[string]bool{}
	for _, zone := range []string{cloudZoneChats, cloudZoneMessages, cloudZoneAttachments} {
		hasToken, lastSuccess, lastErr, updatedAt, err := store.getSyncStateDetail(ctx, zone)
		if err != nil {
			return nil, fmt.Errorf("failed to read sync state for zone %s: %w", zone, err)
		}
		report.Zones = append(report.Zones, ZoneSyncStatus{
			Zone:        zone,
			HasToken:    hasToken,
			LastSuccess: lastSuccess,
			LastError:   lastErr,
			UpdatedAt:   updatedAt,
		})
		zoneDone[zone] = lastSuccess != nil
	}
	report.BootstrapComplete = zoneDone[cloudZoneChats] && zoneDone[cloudZoneMessages]

	if report.TotalChats, err = store.debugTotalChatCount(ctx); err != nil {
		return nil, fmt.Errorf("failed to count chats: %w", err)
	}
	if report.DeliverableMessages, err = store.deliverableMessageCount(ctx); err != nil {
		return nil, fmt.Errorf("failed to count deliverable messages: %w", err)
	}
	if report.DeliveredMessages, err = store.deliveredMessageCount(ctx, bridgeID); err != nil {
		return nil, fmt.Errorf("failed to count delivered messages: %w", err)
	}
	if report.LiveMessageCount, report.LiveMessageLastAt, err = getLiveMessageStats(ctx, db, bridgeID); err != nil {
		return nil, fmt.Errorf("failed to read live-message stats: %w", err)
	}
	if report.StatusKitUpdateCount, report.StatusKitUpdateLastAt, err = getStatusKitUpdateStats(ctx, db, bridgeID); err != nil {
		return nil, fmt.Errorf("failed to read StatusKit update stats: %w", err)
	}
	return report, nil
}

// zoneLabel gives a short human name for a CloudKit zone constant.
func zoneLabel(zone string) string {
	switch zone {
	case cloudZoneChats:
		return "chats"
	case cloudZoneMessages:
		return "messages"
	case cloudZoneAttachments:
		return "attachments"
	default:
		return zone
	}
}

// mostRecentZoneActivity returns the newest UpdatedAt across all zones, used
// to heuristically judge whether a sync is likely active right now when no
// live process state is available (e.g. from the standalone CLI).
func (r *SyncStatusReport) mostRecentZoneActivity() *time.Time {
	var newest *time.Time
	for _, z := range r.Zones {
		if z.UpdatedAt != nil && (newest == nil || z.UpdatedAt.After(*newest)) {
			newest = z.UpdatedAt
		}
	}
	return newest
}

// Format renders the report as management-room / terminal text. liveRunning,
// when non-nil, overrides the "is it running right now" line with the
// process's actual in-memory flag (only available when called from inside
// the running bridge); nil falls back to a recency heuristic.
func (r *SyncStatusReport) Format(liveRunning *bool) string {
	var sb strings.Builder

	sb.WriteString("**CloudKit -> database sync**\n")
	if !r.BootstrapComplete {
		sb.WriteString("Status: ⏳ initial sync not yet complete (Matrix room creation is gated until it finishes)\n")
	} else {
		switch {
		case liveRunning != nil && *liveRunning:
			sb.WriteString("Status: ⏳ actively syncing now\n")
		case liveRunning != nil && !*liveRunning:
			sb.WriteString("Status: ✅ caught up\n")
		default:
			if recent := r.mostRecentZoneActivity(); recent != nil && time.Since(*recent) < 5*time.Minute {
				sb.WriteString(fmt.Sprintf("Status: ⏳ likely active (last activity %s ago)\n", time.Since(*recent).Round(time.Second)))
			} else {
				sb.WriteString("Status: ✅ caught up (no in-progress signal available outside the running bridge; based on last activity time)\n")
			}
		}
	}

	for _, z := range r.Zones {
		last := "never"
		if z.LastSuccess != nil {
			last = fmt.Sprintf("%s ago", time.Since(*z.LastSuccess).Round(time.Second))
		}
		line := fmt.Sprintf("  %-12s last success: %s", zoneLabel(z.Zone), last)
		if z.LastError != "" {
			line += fmt.Sprintf("  ⚠️ last error: %s", z.LastError)
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString(fmt.Sprintf("Chats ingested: %d\n\n", r.TotalChats))

	sb.WriteString("**Database -> Matrix delivery**\n")
	pending := r.PendingMessages()
	pct := 100.0
	if r.DeliverableMessages > 0 {
		pct = 100 * float64(r.DeliveredMessages) / float64(r.DeliverableMessages)
	}
	sb.WriteString(fmt.Sprintf("Delivered: %d / %d messages (%.1f%%), %d pending\n", r.DeliveredMessages, r.DeliverableMessages, pct, pending))
	if r.FullyCaughtUp() {
		sb.WriteString("✅ Fully caught up — no backlog remaining.\n\n")
	} else {
		sb.WriteString("(Reactions aren't counted — they bridge separately and aren't tracked here. \"Pending\" includes messages whose chat has no Matrix room yet, e.g. very old chats never opened in Matrix, not just an active delivery backlog.)\n\n")
	}

	sb.WriteString("**Live traffic (steady-state, since ever)**\n")
	if r.LiveMessageLastAt != nil {
		sb.WriteString(fmt.Sprintf("%d messages handed off for live delivery, most recent %s ago\n", r.LiveMessageCount, time.Since(*r.LiveMessageLastAt).Round(time.Second)))
	} else {
		sb.WriteString("No live messages recorded yet.\n")
	}
	if r.StatusKitUpdateLastAt != nil {
		sb.WriteString(fmt.Sprintf("%d StatusKit-only room updates (Focus/DND, no message content), most recent %s ago\n", r.StatusKitUpdateCount, time.Since(*r.StatusKitUpdateLastAt).Round(time.Second)))
	} else {
		sb.WriteString("No StatusKit-only room updates recorded yet.\n")
	}
	sb.WriteString("(A room bumping to \"recently active\" in your Matrix client can mean either of the two lines above — a StatusKit Focus/DND change rewrites the room name/state with no new message, so it's easy to mistake for backfill. Live message traffic bypasses the CloudKit-sourced counters above until the next CloudKit re-sync.)\n")

	return sb.String()
}
