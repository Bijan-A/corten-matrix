// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

// Contact-based DM portal merging.
//
// When a contact has multiple phone numbers or emails, iMessage stores each
// as a separate conversation. Without merging, the bridge creates separate
// Matrix rooms for each number. This file provides helpers to redirect
// incoming messages from a secondary phone number to an existing primary portal.

import (
	"context"
	"sort"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/lrhodin/corten-matrix/imessage"
)

// resolveContactPortalID checks if the given DM identifier belongs to a contact
// that already has an existing portal under a different phone number or email.
// Returns the original identifier (as a PortalID) if no existing portal is found.
func (c *IMClient) resolveContactPortalID(identifier string) networkid.PortalID {
	defaultID := networkid.PortalID(identifier)

	if strings.Contains(identifier, ",") {
		return defaultID
	}

	// Only handles the guard proves are the same person may capture this DM.
	// Walking the card directly is what let a shared household handle redirect
	// one person's conversation onto another's portal.
	altIDs := c.mutualContactHandles(identifier)
	if len(altIDs) == 0 {
		return defaultID
	}

	ctx := context.Background()
	for _, altID := range altIDs {
		portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
			ID:       networkid.PortalID(altID),
			Receiver: c.UserLogin.ID,
		})
		if err == nil && portal != nil && portal.MXID != "" {
			c.UserLogin.Log.Debug().
				Str("original", identifier).
				Str("resolved", altID).
				Msg("Resolved contact portal to existing portal")
			return networkid.PortalID(altID)
		}
	}

	return defaultID
}

// validateTargetsSafe wraps Client.ValidateTargets with a recover guard.
// The call crosses into the identity-manager FFI path, which has reachable
// panic sites upstream (identity_manager.rs:249/335/542/555); a panic must
// not crash the bridge, so it degrades to "nothing validated" (nil). Shared
// by the send path and user-triggered commands.
func (c *IMClient) validateTargetsSafe(targets []string) (valid []string) {
	if c.client == nil || len(targets) == 0 {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			c.UserLogin.Log.Error().Interface("panic", r).Int("targets", len(targets)).
				Msg("ValidateTargets panicked in FFI path")
			valid = nil
		}
	}()
	return c.client.ValidateTargets(targets, c.handle)
}

// resolveSendTarget determines the best identifier to send to for a DM portal.
func (c *IMClient) resolveSendTarget(portalID string) string {
	if c.client == nil || strings.Contains(portalID, ",") {
		return portalID
	}

	// Guarded alternates only. This is the call site that misdelivered: the
	// shared landline's card linked to a different family member, and the
	// fallback below sent to them.
	alternates := c.mutualContactHandles(portalID)
	if len(alternates) == 0 {
		return portalID
	}

	// Validate the portal's own handle alone first. In the common case it is
	// reachable and this stays a single-handle IDS query; including the
	// alternates here would fetch keys for every handle of the contact on the
	// send path (unregistered handles are only cached for EMPTY_REFRESH = 1h
	// rust-side, so dead alternates would be re-fetched from Apple hourly),
	// and it would couple the portal handle's validation to the alternates'
	// failure domain — one erroring batch would blank out everything.
	if valid := c.validateTargetsSafe([]string{portalID}); len(valid) > 0 {
		return portalID
	}

	c.UserLogin.Log.Info().
		Str("portal_id", portalID).
		Int("alternates", len(alternates)).
		Msg("Portal ID not reachable on iMessage, trying alternate contact numbers")

	valid := c.validateTargetsSafe(alternates)
	validSet := make(map[string]struct{}, len(valid))
	for _, id := range valid {
		validSet[id] = struct{}{}
	}
	if picked, ok := pickSendTarget(portalID, alternates, validSet); ok {
		c.UserLogin.Log.Info().
			Str("portal_id", portalID).
			Str("send_target", picked).
			Int("alternates", len(alternates)).
			Int("valid", len(valid)).
			Msg("Resolved send target to alternate contact number")
		return picked
	}

	c.UserLogin.Log.Warn().
		Str("portal_id", portalID).
		Int("alternates", len(alternates)).
		Msg("No reachable number found for contact; falling back to original portal ID")
	return portalID
}

func pickSendTarget(portalID string, altIDs []string, validSet map[string]struct{}) (string, bool) {
	if _, ok := validSet[portalID]; ok {
		return portalID, true
	}
	for _, altID := range altIDs {
		if altID == portalID {
			continue
		}
		if _, ok := validSet[altID]; ok {
			return altID, true
		}
	}
	return portalID, false
}

// lookupContact resolves a portal/identifier string to a Contact using
// cloud contacts (iCloud CardDAV), falling back to chat.db contacts.
func (c *IMClient) lookupContact(identifier string) *imessage.Contact {
	localID := stripIdentifierPrefix(identifier)
	if localID == "" {
		return nil
	}

	if store := c.contactStore(); store != nil {
		contact, _ := store.GetContactInfo(localID)
		if contact != nil {
			return contact
		}
	}
	if c.chatDB != nil {
		contact, _ := c.chatDB.api.GetContactInfo(localID)
		return contact
	}
	return nil
}

// countNonSelfMembers counts the unique non-self members of a conversation
// across the participant list and the sender, collapsing a contact's alternate
// handles so a multi-number contact isn't double-counted. The group/DM signal
// for inbound routing: self is implicit, so >=2 other members means a group.
// Sender is included because relayed carrier groups omit self from participants.
func (c *IMClient) countNonSelfMembers(participants []string, sender *string) int {
	seen := make(map[string]bool)
	count := 0
	add := func(raw string) {
		n := normalizeIdentifierForPortalID(raw)
		if n == "" || c.isMyHandle(n) || seen[n] {
			return
		}
		seen[n] = true
		count++
		// Guarded alternates only: collapsing through a handle two people
		// share would fold them into one member, and a two-person group
		// counted as one member is routed as a DM.
		for _, altID := range c.mutualContactHandles(n) {
			seen[altID] = true
		}
	}
	for _, p := range participants {
		add(p)
	}
	if sender != nil {
		add(*sender)
	}
	return count
}

// getContactChatGUIDs returns all possible chat.db GUIDs for a DM portal,
// including GUIDs for alternate phone numbers/emails belonging to the same contact.
func (c *IMClient) getContactChatGUIDs(portalID string) []string {
	guids := portalIDToChatGUIDs(portalID)

	for _, altID := range c.mutualContactHandles(portalID) {
		guids = append(guids, portalIDToChatGUIDs(altID)...)
	}

	return guids
}

// contactKeyFromContact returns a stable identity key for grouping a contact's
// DM entries during initial sync deduplication. Returns "" if no merging is
// needed (single phone, no name, etc.).
func contactKeyFromContact(contact *imessage.Contact) string {
	if contact == nil || !contact.HasName() {
		return ""
	}
	phones := make([]string, 0, len(contact.Phones))
	for _, p := range contact.Phones {
		n := normalizePhoneForPortalID(p)
		if n != "" {
			phones = append(phones, n)
		}
	}
	if len(phones) <= 1 {
		return ""
	}
	sort.Strings(phones)
	return strings.Join(phones, "|")
}

// contactPortalIDs returns all portal ID strings for a contact's phone numbers
// and emails.
func contactPortalIDs(contact *imessage.Contact) []string {
	if contact == nil {
		return nil
	}

	seen := make(map[string]bool)
	var ids []string

	for _, phone := range contact.Phones {
		normalized := normalizePhoneForPortalID(phone)
		if normalized == "" {
			continue
		}
		pid := "tel:" + normalized
		if !seen[pid] {
			seen[pid] = true
			ids = append(ids, pid)
		}
	}

	for _, email := range contact.Emails {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			continue
		}
		pid := "mailto:" + email
		if !seen[pid] {
			seen[pid] = true
			ids = append(ids, pid)
		}
	}

	return ids
}

// normalizePhoneForPortalID converts a phone number to E.164-like format.
func normalizePhoneForPortalID(phone string) string {
	n := normalizePhone(phone)
	if n == "" {
		return ""
	}
	if strings.HasPrefix(n, "+") {
		return n
	}
	if len(n) == 10 {
		return "+1" + n
	}
	if len(n) == 11 && n[0] == '1' {
		return "+" + n
	}
	return "+" + n
}

// canonicalContactHandle returns a deterministic canonical handle for a contact
// that has multiple iMessage handles (phone + email). This ensures CloudKit
// backfill creates a single portal per contact rather than one per handle.
// If the identifier doesn't resolve to a multi-handle contact, returns it unchanged.
func (c *IMClient) canonicalContactHandle(identifier string) string {
	mutual := c.mutualContactHandles(identifier)
	if len(mutual) == 0 {
		return identifier
	}
	// identifier is a candidate for its own canonical, so it goes back into the
	// set the sort picks from — mutualContactHandles returns only alternates.
	altIDs := append(append(make([]string, 0, len(mutual)+1), mutual...), identifier)
	sort.Strings(altIDs)
	for _, id := range altIDs {
		if strings.HasPrefix(id, "tel:") {
			return id
		}
	}
	return altIDs[0]
}

// canonicalizeDMSender remaps the sender identity for DM events so that the
// ghost matches the portal's canonical handle. Without this, a contact sending
// from their email handle into a phone-based DM portal causes a phantom ghost
// to briefly join the room.
func (c *IMClient) canonicalizeDMSender(portalKey networkid.PortalKey, sender bridgev2.EventSender) bridgev2.EventSender {
	if sender.IsFromMe {
		return sender
	}
	portalID := string(portalKey.ID)
	// Only remap for DM portals (not groups or gid: portals).
	if strings.Contains(portalID, ",") || strings.HasPrefix(portalID, "gid:") {
		return sender
	}
	canonicalUserID := makeUserID(portalID)
	if sender.Sender != canonicalUserID {
		return bridgev2.EventSender{
			IsFromMe: false,
			Sender:   canonicalUserID,
		}
	}
	return sender
}

// --- Cross-contact merge guard -------------------------------------------
//
// Everything above treats "handle Y appears on the card I found for handle X"
// as proof that X and Y are the same person. That inference breaks when two
// people share a handle, which is ordinary in a household: a couple's cards
// both list the landline, so the landline's card links back to only one of
// them and the other's DM gets dragged onto it. Observed live: two
// people's DMs both resolved onto the shared home number's portal, so one
// room held both conversations, was labelled with whichever name won the phone
// index, and — via resolveSendTarget's alternate fallback — delivered replies
// to the wrong person.
//
// The guard reasons about PEOPLE instead of cards. contactPersonIndex derives,
// once per contact sync, which person owns each handle; a merge is allowed only
// between two handles the same person owns exclusively. A handle two people
// claim is evidence about neither and is never traversed.
//
// Which of the two cards a lookup returns for such a handle is decided by
// last-write-wins as SyncContacts walks the address book in document order, so
// it is stable for a given response but arbitrary: nothing about the data says
// the later card is the right answer, and a server that reorders its response
// or an edit to either card can swap it. Merging through a handle whose owner
// is picked that way is a coin flip, and one that can land differently after a
// re-sync.
//
// Refusing a merge is the safe direction: the worst case is two rooms for one
// contact, which costs nothing but clutter. Wrongly merging is not recoverable
// the same way — this guard prevents new merges but does not split a room that
// already holds two people's conversations, which needs a separate repair.

// contactIdentityKey returns a case-folded identity for a named contact card.
//
// Keyed on the NAME only, deliberately. Handles cannot identify a card's owner
// here: that is exactly the question the index is trying to answer, and using
// them would union two people through the handle they share — the original bug,
// rebuilt one layer down.
func contactIdentityKey(contact *imessage.Contact) string {
	if contact == nil || !contact.HasName() {
		return ""
	}
	// Mirrors Contact.Name()'s precedence, minus its fallback to a raw
	// email/phone — HasName() already excludes the cards that would hit it.
	name := strings.TrimSpace(contact.FirstName + " " + contact.LastName)
	if name == "" {
		name = contact.Nickname
	}
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// Card names are compared for EXACT equality after case-folding, and nothing
// looser. An earlier version tried to unify a person's duplicate cards by
// treating one name as the same as another when their token sets nested
// ("ann" ⊆ "ann example"). That shipped and immediately unified two different
// people whose cards differed only by a parenthetical qualifier — "A. Example"
// and "A. (B.) Example" nest — so a second person's phone number joined the first
// person's handle set, their DM resolved onto that number's portal, and the
// repair moved the room there and tombstoned the other's room. A parenthetical or middle-name qualifier is the single strongest
// signal that two similar names are NOT one person, which is exactly what the
// nesting rule read as agreement.
//
// The cost of exact matching is that a person with two differently-named cards
// (a first-name-only card beside a full one) is treated as two people and their
// handles will not merge — so they may get one room per handle. That is
// non-destructive and reversible. Guessing is not.

// contactPersonIndex answers, for a whole address book, "who owns this handle"
// and "is this handle claimed by more than one person".
type contactPersonIndex struct {
	// owner maps a handle to its person key. Absent when no named card claims
	// the handle, or when more than one person does (see ambiguous).
	owner map[string]string
	// ambiguous holds handles claimed by two or more people. Never merged
	// through, and the fingerprint a retroactive repair would look for.
	ambiguous map[string]struct{}
	// handles maps a person key to every handle they exclusively own, sorted.
	// Read instead of a single card's handle list so a person split across
	// several cards still merges — the case that broke a purely card-local
	// check on real data.
	handles map[string][]string
}

// buildContactPersonIndex groups cards into people, then records each handle's
// claimant.
//
// Two cards are the same person only when they agree on BOTH the case-folded
// name and at least one handle. Name alone is not enough: two strangers who
// share a name would have their handle sets unioned, and then
// resolveContactPortalID would send one of them's DM into the other's portal —
// this file's own bug, reached by a different route. Requiring a shared handle
// is corroboration; a name is a label, a handle is an identity claim.
//
// The cost is that one person whose cards carry disjoint handles reads as two
// people and their handles do not merge, so they may get one room per card.
// That is the same trade this file makes everywhere: an extra room is
// non-destructive and reversible, a wrong merge is neither. On the address book
// this was developed against, 5 names had more than one card — 2 shared a
// handle (still merged) and 3 did not (no longer merged, previously a silent
// cross-card union).
//
// The person key is therefore NOT the name. Two components can share a name, so
// keying on it would collapse exactly the case this guards against; the key is
// the name plus the component's lowest handle, which is unique per component.
//
// The limit of the rule: two DIFFERENT people who share both a name and a
// handle still union, and that handle is not marked ambiguous — the household
// shape with a name collision on top. Nothing in an address book distinguishes
// that from one person listed on two cards, which is the same reason a
// corroborated union is trusted at all. It is strictly better than keying on
// the name alone, not airtight.
func buildContactPersonIndex(contacts []*imessage.Contact) *contactPersonIndex {
	idx := &contactPersonIndex{
		owner:     make(map[string]string),
		ambiguous: make(map[string]struct{}),
		handles:   make(map[string][]string),
	}

	type cardInfo struct {
		name    string
		handles []string
	}
	cards := make([]cardInfo, 0, len(contacts))
	for _, contact := range contacts {
		// Unnamed cards carry no identity to compare and never drive a merge
		// (every caller gates on HasName).
		name := contactIdentityKey(contact)
		if name == "" {
			continue
		}
		handles := contactPortalIDs(contact)
		if len(handles) == 0 {
			continue
		}
		cards = append(cards, cardInfo{name: name, handles: handles})
	}

	// Union-find over cards. Only cards that already share a name are ever
	// compared, so this stays near-linear instead of quadratic over the book.
	parent := make([]int, len(cards))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		// Lowest index wins, so a component's representative does not depend on
		// the order the shared handles happened to be discovered in.
		if ra < rb {
			parent[rb] = ra
		} else {
			parent[ra] = rb
		}
	}
	// Within one name, the first card seen claiming a handle owns it; any later
	// card claiming the same handle joins that card's component.
	claimedWithinName := make(map[string]int, len(cards))
	for i, card := range cards {
		for _, handle := range card.handles {
			key := card.name + "\x00" + handle
			if j, seen := claimedWithinName[key]; seen {
				union(i, j)
			} else {
				claimedWithinName[key] = i
			}
		}
	}

	// Collect each component's handles, then name it.
	componentHandles := make(map[int][]string)
	for i, card := range cards {
		root := find(i)
		componentHandles[root] = append(componentHandles[root], card.handles...)
	}
	personKey := make(map[int]string, len(componentHandles))
	for root, handles := range componentHandles {
		low := ""
		for _, h := range handles {
			if low == "" || h < low {
				low = h
			}
		}
		personKey[root] = cards[root].name + "|" + low
	}

	claimants := make(map[string]map[string]struct{})
	for i := range cards {
		person := personKey[find(i)]
		for _, handle := range cards[i].handles {
			set := claimants[handle]
			if set == nil {
				set = make(map[string]struct{}, 1)
				claimants[handle] = set
			}
			set[person] = struct{}{}
		}
	}
	for handle, people := range claimants {
		if len(people) > 1 {
			// Claimed by two components — two different people as far as this
			// index can tell. Never merged through, in either direction.
			idx.ambiguous[handle] = struct{}{}
			continue
		}
		for person := range people {
			idx.owner[handle] = person
			idx.handles[person] = append(idx.handles[person], handle)
		}
	}
	for person, hs := range idx.handles {
		idx.handles[person] = sortContactHandles(dedupeHandles(hs))
	}
	return idx
}

// dedupeHandles removes repeats, which arise when several cards in one
// component list the same handle (the shared handle that joined them).
func dedupeHandles(handles []string) []string {
	seen := make(map[string]struct{}, len(handles))
	out := handles[:0]
	for _, h := range handles {
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	return out
}

// contactPersonIndex returns the cached index for the installed contact source,
// rebuilding it when the source reports a different cache generation. The
// returned value is shared between callers and must be treated as read-only.
//
// Keyed on CacheStatus rather than a timer so it can never disagree with the
// lookups it guards: a sync that adds a second claimant to a handle changes
// (count, lastSync), which invalidates this. Two callers racing a rebuild both
// compute the same index, so the duplicated work is harmless.
func (c *IMClient) contactPersonIndex() *contactPersonIndex {
	store := c.contactStore()
	if store == nil {
		return nil
	}
	count, lastSync := store.CacheStatus()

	c.personIndexMu.RLock()
	if c.personIndex != nil && c.personIndexContacts == count && c.personIndexSync.Equal(lastSync) {
		cached := c.personIndex
		c.personIndexMu.RUnlock()
		return cached
	}
	c.personIndexMu.RUnlock()

	idx := buildContactPersonIndex(store.GetAllContacts())

	c.personIndexMu.Lock()
	c.personIndex = idx
	c.personIndexContacts = count
	c.personIndexSync = lastSync
	c.personIndexMu.Unlock()
	return idx
}

// sortContactHandles orders a person's handles tel: first, then mailto:, each
// group lexicographically.
//
// The order is load-bearing, not cosmetic: resolveContactPortalID adopts the
// FIRST alternate that already has a live portal, so this decides which of a
// contact's rooms becomes the one their messages land in. The pre-guard code
// walked the raw card, whose phones precede its emails, so phone portals won
// — and on a live bridge one contact's DM had resolved to her phone portal 192
// times. Sorting the whole set as plain strings puts "mailto:" before "tel:"
// and silently moved her (and every multi-handle contact) to an email room.
//
// Phone-first also matches canonicalContactHandle's own tel: preference and the
// phone_numbers_in_profile override in IMConnector.Start, which exists so a DM's
// contact card carries a dialable number.
func sortContactHandles(handles []string) []string {
	out := append([]string(nil), handles...)
	sort.SliceStable(out, func(i, j int) bool {
		iTel := strings.HasPrefix(out[i], "tel:")
		jTel := strings.HasPrefix(out[j], "tel:")
		if iTel != jTel {
			return iTel
		}
		return out[i] < out[j]
	})
	return out
}

// contactHandleOwner reports which person owns a handle. ambiguous is true when
// two or more people claim it, in which case owner is empty and no merge through
// the handle is permitted.
func (c *IMClient) contactHandleOwner(handle string) (owner string, ambiguous bool) {
	idx := c.contactPersonIndex()
	if idx == nil {
		return "", false
	}
	if _, amb := idx.ambiguous[handle]; amb {
		return "", true
	}
	return idx.owner[handle], false
}

// mutualContactHandles returns the handles that provably belong to the same
// person as identifier, excluding identifier itself. Returns nil when no merge
// is justified — no card, an ambiguous handle, or a person with nothing else.
//
// This is the one chokepoint for contact-based handle merging; every caller that
// used to walk contactPortalIDs directly goes through here, so a card overlap
// cannot reach a portal key, a send target or a member count.
func (c *IMClient) mutualContactHandles(identifier string) []string {
	idx := c.contactPersonIndex()
	if idx == nil {
		return nil
	}
	if _, amb := idx.ambiguous[identifier]; amb {
		return nil
	}
	person := idx.owner[identifier]
	if person == "" {
		return nil
	}
	owned := idx.handles[person]
	if len(owned) <= 1 {
		return nil
	}
	mutual := make([]string, 0, len(owned)-1)
	for _, alt := range owned {
		if alt != identifier {
			mutual = append(mutual, alt)
		}
	}
	if len(mutual) == 0 {
		return nil
	}
	return mutual
}
