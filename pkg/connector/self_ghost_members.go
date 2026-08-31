// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// Keeping the ghosts for the user's OWN handles in room member lists.

package connector

import (
	"context"
	"strconv"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	matrixfmt "maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// selfGhostMemberKeySuffix distinguishes the plain-ghost member entry for one of
// our own handles from the IsFromMe entry for the same handle.
//
// bridgev2 identifies a member by ChatMember.EventSender (syncParticipants
// iterates MemberMap's VALUES; the key only dedupes), so two entries may share a
// Sender as long as their keys differ. That is exactly the shape we need: one
// remote participant — me — present in the room as BOTH my Matrix account and
// the ghost that authored my history there. The suffix contains a character that
// cannot appear in a bridge user ID (those are "tel:+…" / "mailto:…"), so it can
// never collide with a real member.
const selfGhostMemberKeySuffix = "\x00self-ghost"

// selfGhostRoomsRetryCooldown throttles retries after a failed load, so a query
// that fails does so once per cooldown rather than once per portal.
const selfGhostRoomsRetryCooldown = 5 * time.Minute

// selfGhostRooms returns the set of own-handle ghosts that have authored
// messages in each portal, keyed by portal ID.
//
// Why this exists: our own handles are mapped to the logged-in Matrix user
// (IsFromMe), and for an IsFromMe member bridgev2 syncs the double puppet and
// returns before touching the ghost — so the ghost for that handle is never in
// the synced set, and ChatMemberList.IsFull then removes it with "User is not in
// remote chat". But that ghost is a legitimate author: it wrote the history in
// the room, and forward backfill re-joins it (POST /join) to write more. The
// result was a visible leave-then-rejoin of a ghost carrying the user's OWN name
// and avatar, in their self-chats and in every DM/group where they had sent
// messages before double puppeting was configured — on every single startup.
//
// Two signals are unioned, because either alone leaves rooms exposed:
//   - ghosts that AUTHORED messages in the portal (one DB query). This is the
//     set that must never be evicted: forward backfill re-joins such a ghost to
//     write history, so a kick just starts a leave/rejoin cycle.
//   - ghosts CURRENTLY JOINED to the portal's room (one /joined_rooms per own
//     handle). A ghost can be joined without having authored anything — seen in
//     the wild on two shortcode DMs, where the message-only signal missed it and
//     the member sync evicted it.
//
// Loaded once per connect (resetSelfGhostRooms clears it) rather than per
// portal: GetChatInfo runs for every portal on a startup resync (thousands), and
// within a connect the answer only changes when an own-handle ghost writes in or
// joins a NEW room — either way a live action that joins the ghost anyway, and
// one the next connect's load picks up.
func (c *IMClient) selfGhostRooms(ctx context.Context) map[string]map[networkid.UserID]bool {
	c.selfGhostRoomsMu.Lock()
	defer c.selfGhostRoomsMu.Unlock()
	if c.selfGhostRoomsLoaded {
		return c.selfGhostRoomsCache
	}

	if !c.selfGhostRoomsRetryAt.IsZero() && time.Now().Before(c.selfGhostRoomsRetryAt) {
		// A recent load failed. Don't re-run the query for every portal of a
		// startup resync (thousands of calls); wait out the cooldown.
		return nil
	}

	ownGhostIDs := c.ownGhostIDs()
	if len(ownGhostIDs) == 0 {
		c.selfGhostRoomsLoaded = true
		return nil
	}

	query, args := buildSelfGhostRoomsQuery(c.Main.Bridge.ID, c.UserLogin.ID, ownGhostIDs)
	rows, err := c.Main.Bridge.DB.Database.Query(ctx, query, args...)
	if err != nil {
		// Back off instead of failing open per portal: without the keep set the
		// member sync evicts own-handle ghosts again, but re-running a failing
		// query once per GetChatInfo would do that thousands of times a startup.
		c.selfGhostRoomsRetryAt = time.Now().Add(selfGhostRoomsRetryCooldown)
		c.UserLogin.Log.Warn().Err(err).Dur("retry_in", selfGhostRoomsRetryCooldown).
			Msg("Failed to load rooms where own-handle ghosts authored messages")
		return nil
	}
	defer rows.Close()

	result := make(map[string]map[networkid.UserID]bool)
	for rows.Next() {
		var roomID, senderID string
		if err := rows.Scan(&roomID, &senderID); err != nil {
			c.UserLogin.Log.Warn().Err(err).Msg("Failed to scan own-handle ghost room row")
			continue
		}
		if result[roomID] == nil {
			result[roomID] = make(map[networkid.UserID]bool)
		}
		result[roomID][networkid.UserID(senderID)] = true
	}
	if err := rows.Err(); err != nil {
		c.selfGhostRoomsRetryAt = time.Now().Add(selfGhostRoomsRetryCooldown)
		c.UserLogin.Log.Warn().Err(err).Msg("Own-handle ghost room row iteration error")
		return nil
	}

	authored := len(result)
	result = mergeSelfGhostRooms(result, c.selfGhostJoinedRooms(ctx, ownGhostIDs))

	c.selfGhostRoomsCache = result
	c.selfGhostRoomsLoaded = true
	c.UserLogin.Log.Debug().
		Int("portals", len(result)).
		Int("from_authored_messages", authored).
		Msg("Loaded portals whose member list must keep a ghost for one of our own handles")
	return result
}

// buildSelfGhostRoomsQuery returns the authored-messages lookup and its binds.
//
// The handle list is expanded into explicit placeholders rather than passed to
// "= ANY($3)": ANY is Postgres-only — SQLite answers "no such function: ANY" —
// and SQLite is a first-class backend here. dbutil rewrites "$N" to "?N" for
// SQLite, so numbered placeholders are portable as long as the list is expanded
// rather than bound as one array. Own handles number a handful, so the IN list
// stays tiny.
func buildSelfGhostRoomsQuery(bridgeID networkid.BridgeID, loginID networkid.UserLoginID, ghostIDs []networkid.UserID) (string, []any) {
	if len(ghostIDs) == 0 {
		// An empty list would emit "IN ()", a syntax error on both dialects.
		// The caller guards on this already; returning nothing keeps the
		// function safe to call on its own terms now that it is testable.
		return "", nil
	}
	args := make([]any, 0, len(ghostIDs)+2)
	args = append(args, bridgeID, loginID)
	placeholders := make([]string, len(ghostIDs))
	for i, id := range ghostIDs {
		placeholders[i] = "$" + strconv.Itoa(i+3)
		args = append(args, string(id))
	}
	return `SELECT DISTINCT room_id, sender_id FROM message
		 WHERE bridge_id=$1 AND room_receiver=$2 AND sender_id IN (` + strings.Join(placeholders, ", ") + `)`, args
}

// resetSelfGhostRooms drops the cached keep set so the next lookup reloads it.
// Called from Connect: without this the set is loaded once per PROCESS, and a
// reconnect (or a re-login) would keep serving a snapshot from before it.
func (c *IMClient) resetSelfGhostRooms() {
	c.selfGhostRoomsMu.Lock()
	c.selfGhostRoomsCache = nil
	c.selfGhostRoomsLoaded = false
	c.selfGhostRoomsRetryAt = time.Time{}
	c.selfGhostRoomsMu.Unlock()
}

// mergeSelfGhostRooms folds src into dst, unioning the ghost sets per portal, and
// returns the result. Either side may be nil: the authored-messages query and the
// /joined_rooms enumeration each cover rooms the other misses, and either can
// come back empty (no history yet, or a homeserver call that failed).
func mergeSelfGhostRooms(dst, src map[string]map[networkid.UserID]bool) map[string]map[networkid.UserID]bool {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]map[networkid.UserID]bool, len(src))
	}
	for portalID, ghosts := range src {
		if dst[portalID] == nil {
			dst[portalID] = make(map[networkid.UserID]bool, len(ghosts))
		}
		for ghostID := range ghosts {
			dst[portalID][ghostID] = true
		}
	}
	return dst
}

// selfGhostJoinedRooms asks the homeserver which rooms each own-handle ghost is
// actually joined to and maps them back to portal IDs. One /joined_rooms request
// per own handle (a handful), masquerading as that ghost — not per portal.
//
// This is the precise form of the invariant: the member sync only evicts members
// that are currently joined, so enumerating them is what makes the protection
// complete. The authored-messages query stays as the primary signal because it
// also covers a ghost that SHOULD be joined but currently isn't (backfill will
// join it to write history), which /joined_rooms by definition cannot report.
//
// A failure for one handle is logged and skipped rather than retried per portal:
// the union degrades to the authored-messages signal for that handle, and the
// next connect re-enumerates.
func (c *IMClient) selfGhostJoinedRooms(ctx context.Context, ownGhostIDs []networkid.UserID) map[string]map[networkid.UserID]bool {
	portals, err := c.Main.Bridge.GetAllPortalsWithMXID(ctx)
	if err != nil {
		c.UserLogin.Log.Warn().Err(err).Msg("Failed to load portals to map own-handle ghost rooms")
		return nil
	}
	byMXID := make(map[id.RoomID]string, len(portals))
	for _, portal := range portals {
		if portal.Receiver == c.UserLogin.ID && portal.MXID != "" {
			byMXID[portal.MXID] = string(portal.ID)
		}
	}
	if len(byMXID) == 0 {
		return nil
	}

	result := make(map[string]map[networkid.UserID]bool)
	for _, ghostID := range ownGhostIDs {
		ghost, err := c.Main.Bridge.GetGhostByID(ctx, ghostID)
		if err != nil || ghost == nil {
			c.UserLogin.Log.Warn().Err(err).Str("ghost_id", string(ghostID)).
				Msg("Failed to load own-handle ghost to enumerate its rooms")
			continue
		}
		asIntent, ok := ghost.Intent.(*matrixfmt.ASIntent)
		if !ok {
			// Not the appservice connector (or a test double) — the
			// authored-messages signal carries the protection alone.
			return result
		}
		resp, err := asIntent.Matrix.JoinedRooms(ctx)
		if err != nil {
			c.UserLogin.Log.Warn().Err(err).Str("ghost_id", string(ghostID)).
				Msg("Failed to list rooms joined by an own-handle ghost")
			continue
		}
		joined := 0
		for _, roomID := range resp.JoinedRooms {
			portalID, ok := byMXID[roomID]
			if !ok {
				continue
			}
			if result[portalID] == nil {
				result[portalID] = make(map[networkid.UserID]bool, 1)
			}
			result[portalID][ghostID] = true
			joined++
		}
		c.UserLogin.Log.Debug().Str("ghost_id", string(ghostID)).
			Int("joined_portals", joined).
			Int("joined_rooms_total", len(resp.JoinedRooms)).
			Msg("Enumerated rooms joined by an own-handle ghost")
	}
	return result
}

// ownGhostIDs returns the ghost user IDs for every handle registered to this
// login (the primary send handle plus every alias).
func (c *IMClient) ownGhostIDs() []networkid.UserID {
	seen := make(map[networkid.UserID]bool, len(c.allHandles)+1)
	out := make([]networkid.UserID, 0, len(c.allHandles)+1)
	for _, handle := range append([]string{c.handle}, c.allHandles...) {
		normalized := normalizeIdentifierForPortalID(handle)
		if normalized == "" {
			continue
		}
		id := makeUserID(normalized)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// addSelfGhostMembers adds a plain (non-IsFromMe) member entry for every
// own-handle ghost that authored messages in this portal, so a full member sync
// keeps it joined instead of kicking it. See selfGhostRooms for why.
//
// Entries already present as plain ghosts are left alone; an IsFromMe entry for
// the same handle is kept (it is what keeps the user's own Matrix account in the
// room, which matters when kick_matrix_users is enabled) and the ghost is added
// alongside it under a suffixed key.
func (c *IMClient) addSelfGhostMembers(ctx context.Context, portalID string, memberMap map[networkid.UserID]bridgev2.ChatMember) {
	if memberMap == nil {
		return
	}
	ghosts := c.selfGhostRooms(ctx)[portalID]
	if len(ghosts) == 0 {
		return
	}
	for ghostID := range ghosts {
		entry := bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{Sender: ghostID},
			Membership:  event.MembershipJoin,
		}
		existing, ok := memberMap[ghostID]
		if !ok {
			memberMap[ghostID] = entry
			continue
		}
		if !existing.IsFromMe {
			// Already synced as a plain ghost — nothing to protect it from.
			continue
		}
		memberMap[networkid.UserID(string(ghostID)+selfGhostMemberKeySuffix)] = entry
	}
}

// isSelfGhostMemberKey reports whether a member map key is one of the suffixed
// keys added by addSelfGhostMembers. Only used by tests and diagnostics; the
// suffix is never sent to Matrix.
func isSelfGhostMemberKey(key networkid.UserID) bool {
	return strings.HasSuffix(string(key), selfGhostMemberKeySuffix)
}
