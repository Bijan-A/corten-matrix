// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// Read-only diagnostic for conversations bridged into more than one portal.

package connector

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"maunium.net/go/mautrix/id"
)

// splitConversationShard is one portal that a single iMessage conversation has
// been bridged into.
type splitConversationShard struct {
	PortalID string
	Messages int
	MXID     id.RoomID
}

// splitConversation is one iMessage conversation (identified by its group_id)
// that exists as more than one portal.
//
// group_id is iMessage's own per-conversation identifier, carried through from
// the CloudKit chat record, so "same group_id" cannot false-merge two distinct
// people or two distinct threads the way a handle- or participant-derived key
// can. The inverse — one conversation whose group_id rotated — is what the
// participant-key path in consolidateGroupPortals already covers, so this
// reports the axis that path misses.
type splitConversation struct {
	GroupID string
	// Shards, most messages first, then by portal ID so the report is stable.
	Shards []splitConversationShard
}

// IsDirect reports whether this conversation's portals are 1:1 chats.
//
// Group portals are keyed either "gid:<uuid>" or by a comma-joined participant
// set (see listGroupChats, which selects on exactly those two shapes); anything
// else is a direct chat keyed by a single handle. Direct splits are the
// interesting case: consolidateGroupPortals does not consider them at all, so a
// contact reached by both a phone number and an email becomes two rooms each
// holding half the history.
func (s splitConversation) IsDirect() bool {
	for _, shard := range s.Shards {
		if strings.HasPrefix(shard.PortalID, "gid:") || strings.Contains(shard.PortalID, ",") {
			return false
		}
	}
	return true
}

// PopulatedShards counts the portals that actually hold messages.
func (s splitConversation) PopulatedShards() int {
	n := 0
	for _, shard := range s.Shards {
		if shard.Messages > 0 {
			n++
		}
	}
	return n
}

// NeedsRebuild reports whether merging this conversation would require building
// a fresh room rather than moving one.
//
// With a single populated shard the others are empty shells and the existing
// budgeted room-move path can absorb them. With two or more populated shards
// the content is partitioned across rooms, and on a homeserver without
// MSC2716 batch-send historical events can only be APPENDED — there is no way
// to splice one room's messages into the middle of another's timeline. Such a
// merge has to re-backfill into a fresh room oldest-to-newest, which is a much
// larger operation and is why this is only a report for now.
func (s splitConversation) NeedsRebuild() bool {
	return s.PopulatedShards() > 1
}

// splitConversationRow is one (group_id, portal_id) pair as stored, with the
// iCloud "Filtered" (unknown-sender/junk) flag that decides whether the bridge
// would ever give that portal a room.
type splitConversationRow struct {
	GroupID  string
	PortalID string
	Filtered bool
}

// splitConversationReport is what the diagnostic returns.
type splitConversationReport struct {
	Splits []splitConversation
	// FilteredPortalsExcluded counts portals left out because iCloud marks
	// every one of their chats "Filtered" and bridge_filtered_chats is off, so
	// the bridge never creates a room for them. Surfaced rather than silently
	// dropped: "you have a filtered sibling" is a different situation from
	// "this conversation is whole", and the reader should be able to tell.
	FilteredPortalsExcluded int
}

// filterUnbridgedPortals drops rows belonging to portals the bridge would never
// create a room for, so a chat that was never meant to be bridged is not
// reported as a duplicate of one that was.
//
// Mirrors listPortalIDsWithNewestTimestamp's rule, which is per-PORTAL and not
// per-row: participant-set keying can collapse two distinct iMessage chats onto
// one portal_id, one filtered and one not, and such a portal still bridges. So a
// portal is only excluded when every one of its live rows is filtered. When
// bridge_filtered_chats is on, is_filtered is ignored entirely, exactly as it is
// there.
func filterUnbridgedPortals(rows []splitConversationRow, bridgeFilteredChats bool) (kept []splitConversationRow, excludedPortals int) {
	if bridgeFilteredChats {
		return rows, 0
	}
	anyUnfiltered := make(map[string]bool)
	for _, row := range rows {
		if !row.Filtered {
			anyUnfiltered[row.PortalID] = true
		}
	}
	excluded := make(map[string]bool)
	kept = make([]splitConversationRow, 0, len(rows))
	for _, row := range rows {
		if anyUnfiltered[row.PortalID] {
			kept = append(kept, row)
			continue
		}
		excluded[row.PortalID] = true
	}
	return kept, len(excluded)
}

// groupSplitConversations folds flat rows into one entry per group_id that maps
// to more than one distinct portal, dropping the conversations that are already
// whole. Pure logic, so the "which of these is actually split" rule is testable
// without a database.
//
// Duplicate (group_id, portal_id) pairs are expected: several cloud_chat rows
// can share a portal, which is the intended one-room-per-participant-set
// design, and is NOT a split.
func groupSplitConversations(rows []splitConversationRow) []splitConversation {
	byGroup := make(map[string]map[string]bool)
	for _, row := range rows {
		if row.GroupID == "" || row.PortalID == "" {
			continue
		}
		if byGroup[row.GroupID] == nil {
			byGroup[row.GroupID] = make(map[string]bool)
		}
		byGroup[row.GroupID][row.PortalID] = true
	}

	groupIDs := make([]string, 0, len(byGroup))
	for groupID, portals := range byGroup {
		if len(portals) > 1 {
			groupIDs = append(groupIDs, groupID)
		}
	}
	sort.Strings(groupIDs)

	out := make([]splitConversation, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		portals := make([]string, 0, len(byGroup[groupID]))
		for portalID := range byGroup[groupID] {
			portals = append(portals, portalID)
		}
		sort.Strings(portals)
		shards := make([]splitConversationShard, 0, len(portals))
		for _, portalID := range portals {
			shards = append(shards, splitConversationShard{PortalID: portalID})
		}
		out = append(out, splitConversation{GroupID: groupID, Shards: shards})
	}
	return out
}

// sortShards orders a conversation's shards most-populated first, then by portal
// ID, so the report reads as "this is the one holding the history, these are the
// strays" and is stable across runs.
func sortShards(shards []splitConversationShard) {
	sort.Slice(shards, func(i, j int) bool {
		if shards[i].Messages != shards[j].Messages {
			return shards[i].Messages > shards[j].Messages
		}
		return shards[i].PortalID < shards[j].PortalID
	})
}

// findSplitConversations reports every iMessage conversation bridged into more
// than one portal, with each shard's message count and Matrix room.
//
// Read-only: it writes nothing and changes no state. It exists to size the
// problem described in issue #10 before any merging is attempted, and to let a
// repair be verified afterwards.
//
// bridgeFilteredChats mirrors IMConfig.BridgeFilteredChats so the report
// describes the portals this bridge actually creates: with it off (the
// default), a chat iCloud marks "Filtered" never gets a room, and counting it
// as a duplicate of one that did would be a false positive.
//
// Deliberately three small queries rather than one join. cloud_chat is small,
// so the discovery pass is cheap; the counts and room lookups are then scoped
// to just the handful of portals actually involved, which keeps them on the
// (login_id, portal_id, ...) index instead of aggregating over the whole of
// cloud_message — hundreds of thousands of rows on a real account.
func (s *cloudBackfillStore) findSplitConversations(ctx context.Context, bridgeID string, bridgeFilteredChats bool) (splitConversationReport, error) {
	var report splitConversationReport
	rows, err := s.db.Query(ctx, `
		SELECT group_id, portal_id, COALESCE(is_filtered, 0) FROM cloud_chat
		 WHERE login_id=$1 AND deleted=FALSE AND group_id <> '' AND portal_id <> ''`,
		s.loginID,
	)
	if err != nil {
		return report, err
	}
	var flat []splitConversationRow
	for rows.Next() {
		var row splitConversationRow
		var filtered int
		if err := rows.Scan(&row.GroupID, &row.PortalID, &filtered); err != nil {
			rows.Close()
			return report, err
		}
		row.Filtered = filtered != 0
		flat = append(flat, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}

	flat, report.FilteredPortalsExcluded = filterUnbridgedPortals(flat, bridgeFilteredChats)
	splits := groupSplitConversations(flat)
	if len(splits) == 0 {
		return report, nil
	}

	portalIDs := make([]string, 0, len(splits)*2)
	for _, split := range splits {
		for _, shard := range split.Shards {
			portalIDs = append(portalIDs, shard.PortalID)
		}
	}

	counts, err := s.messageCountsByPortal(ctx, portalIDs)
	if err != nil {
		return report, err
	}
	mxids, err := s.roomIDsByPortal(ctx, bridgeID, portalIDs)
	if err != nil {
		return report, err
	}
	for i := range splits {
		for j := range splits[i].Shards {
			shard := &splits[i].Shards[j]
			shard.Messages = counts[shard.PortalID]
			shard.MXID = mxids[shard.PortalID]
		}
		sortShards(splits[i].Shards)
	}
	report.Splits = splits
	return report, nil
}

// inPlaceholders renders a portable "IN ($3, $4, ...)" list starting at the
// given 1-based argument position, along with the values to bind.
//
// The handles are expanded into individual numbered placeholders rather than
// bound as one array: "= ANY($n)" is Postgres-only (SQLite answers "no such
// function: ANY"), while dbutil rewrites "$N" to "?N" for SQLite, so numbered
// placeholders are portable as long as the list is expanded. Returns ok=false
// for an empty list, since "IN ()" is a syntax error on both dialects.
func inPlaceholders(values []string, startAt int) (clause string, args []any, ok bool) {
	if len(values) == 0 {
		return "", nil, false
	}
	seen := make(map[string]bool, len(values))
	placeholders := make([]string, 0, len(values))
	args = make([]any, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		placeholders = append(placeholders, "$"+strconv.Itoa(startAt+len(args)))
		args = append(args, value)
	}
	return "(" + strings.Join(placeholders, ", ") + ")", args, true
}

// messageCountsByPortal returns the number of live messages in each of the
// given portals. Portals with no rows are absent from the map, which the caller
// reads as zero.
func (s *cloudBackfillStore) messageCountsByPortal(ctx context.Context, portalIDs []string) (map[string]int, error) {
	clause, inArgs, ok := inPlaceholders(portalIDs, 2)
	if !ok {
		return map[string]int{}, nil
	}
	args := append([]any{s.loginID}, inArgs...)
	rows, err := s.db.Query(ctx, `
		SELECT portal_id, COUNT(*) FROM cloud_message
		 WHERE login_id=$1 AND deleted=FALSE AND portal_id IN `+clause+`
		 GROUP BY portal_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var portalID string
		var count int
		if err := rows.Scan(&portalID, &count); err != nil {
			return nil, err
		}
		out[portalID] = count
	}
	return out, rows.Err()
}

// roomIDsByPortal returns the Matrix room for each of the given portals that
// has one. A portal with no room is absent from the map.
func (s *cloudBackfillStore) roomIDsByPortal(ctx context.Context, bridgeID string, portalIDs []string) (map[string]id.RoomID, error) {
	clause, inArgs, ok := inPlaceholders(portalIDs, 3)
	if !ok {
		return map[string]id.RoomID{}, nil
	}
	args := append([]any{s.loginID, bridgeID}, inArgs...)
	rows, err := s.db.Query(ctx, `
		SELECT id, mxid FROM portal
		 WHERE receiver=$1 AND bridge_id=$2 AND mxid <> '' AND id IN `+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]id.RoomID)
	for rows.Next() {
		var portalID, mxid string
		if err := rows.Scan(&portalID, &mxid); err != nil {
			return nil, err
		}
		out[portalID] = id.RoomID(mxid)
	}
	return out, rows.Err()
}
