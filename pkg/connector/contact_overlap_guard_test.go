// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for the cross-contact merge guard: two people sharing a handle (a
// household landline) must never be merged into one portal, send target or
// member count, while a genuine multi-handle contact still merges — including
// one whose handles are split across several address-book cards.

package connector

import (
	"reflect"
	"testing"
	"time"

	"github.com/lrhodin/corten-matrix/imessage"
)

// The live shape this guard exists for, with placeholder handles: two people
// whose cards each list their own mobile plus the same shared home number.
const (
	parentAMobile = "tel:+15555550101"
	parentBMobile = "tel:+15555550102"
	sharedLine    = "tel:+15555550199"
	soloMobile    = "tel:+15555550103"
	soloEmail     = "mailto:casey@example.com"
)

func card(first, last string, phones []string, emails []string) *imessage.Contact {
	return &imessage.Contact{FirstName: first, LastName: last, Phones: phones, Emails: emails}
}

// householdBook is the overlapping address book: two people sharing a line,
// plus a one-person phone+email contact.
func householdBook() []*imessage.Contact {
	return []*imessage.Contact{
		card("Avery", "Example", []string{"+15555550101", "+15555550199"}, nil),
		card("Blake", "Example", []string{"+15555550102", "+15555550199"}, nil),
		card("Casey", "Example", []string{"+15555550103"}, []string{"casey@example.com"}),
	}
}

// bookClient installs a contact source whose index maps each handle to the FIRST
// card listing it, mirroring the real client closely enough for these tests. The
// guard reads GetAllContacts, so which card wins a lookup must not matter.
func bookClient(book []*imessage.Contact) *IMClient {
	contacts := map[string]*imessage.Contact{}
	for _, c := range book {
		for _, p := range c.Phones {
			if _, taken := contacts[p]; !taken {
				contacts[p] = c
			}
		}
		for _, e := range c.Emails {
			if _, taken := contacts[e]; !taken {
				contacts[e] = c
			}
		}
	}
	return testNameClient(&fakeContactSource{
		contacts: contacts,
		count:    len(book),
		lastSync: time.Now(),
	})
}

func TestContactIdentityKey(t *testing.T) {
	cases := []struct {
		name    string
		contact *imessage.Contact
		want    string
	}{
		{"nil card", nil, ""},
		{"unnamed card carries no identity", &imessage.Contact{Phones: []string{"+15555550101"}}, ""},
		{"first and last", card("Avery", "Example", nil, nil), "avery example"},
		{"case and spacing folded", card("  AVERY ", "Example  ", nil, nil), "avery example"},
		{"first only", card("Avery", "", nil, nil), "avery"},
		{"nickname when no first/last", &imessage.Contact{Nickname: "Grandma"}, "grandma"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contactIdentityKey(tc.contact); got != tc.want {
				t.Errorf("contactIdentityKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The shared line is claimed by two people and must be owned by neither; each
// person's own mobile stays theirs.
func TestBuildContactPersonIndexMarksSharedHandleAmbiguous(t *testing.T) {
	idx := buildContactPersonIndex(householdBook())

	if _, amb := idx.ambiguous[sharedLine]; !amb {
		t.Errorf("ambiguous[%s] missing — a handle two people list belongs to neither", sharedLine)
	}
	if owner := idx.owner[sharedLine]; owner != "" {
		t.Errorf("owner[%s] = %q, want empty for an ambiguous handle", sharedLine, owner)
	}
	for _, own := range []string{parentAMobile, parentBMobile, soloMobile, soloEmail} {
		if _, amb := idx.ambiguous[own]; amb {
			t.Errorf("ambiguous[%s] set — only one person lists it", own)
		}
		if idx.owner[own] == "" {
			t.Errorf("owner[%s] empty — should belong to its one claimant", own)
		}
	}
	if idx.owner[parentAMobile] == idx.owner[parentBMobile] {
		t.Error("the two people must not share a person key")
	}
}

// Two differently-named cards are two people, even when the names are similar
// and even when they really are one person. Exact-name matching is deliberate:
// see the comment above buildContactPersonIndex.
func TestBuildContactPersonIndexTreatsDifferentNamesAsDifferentPeople(t *testing.T) {
	book := []*imessage.Contact{
		card("Dana", "", []string{"+15555550105"}, nil),
		card("Dana", "Example", nil, []string{"dana@example.com"}),
	}

	idx := buildContactPersonIndex(book)

	phone, email := "tel:+15555550105", "mailto:dana@example.com"
	if idx.owner[phone] == idx.owner[email] {
		t.Errorf("owner[%s] == owner[%s] (%q) — differently-named cards must not be unified",
			phone, email, idx.owner[phone])
	}
	// Conservative fallout: no merge between them. Two rooms for one contact is
	// the accepted cost.
	if got := buildContactPersonIndex(book).handles[idx.owner[phone]]; len(got) != 1 {
		t.Errorf("handles[person] = %v, want only its own card's handle", got)
	}
}

// TestBuildContactPersonIndexKeepsParentheticalQualifiedNamesApart is the
// regression test for the incident this guard caused before it was corrected.
//
// A nesting rule ("avery example" ⊆ "avery (jordan) example") unified two
// different people, so the second person's number entered the first's handle
// set, their DM resolved onto that number's portal, and the repair moved a
// 5,438-message room onto an unrelated key and tombstoned that room. If these
// two ever share a person key again, that damage is live.
func TestBuildContactPersonIndexKeepsParentheticalQualifiedNamesApart(t *testing.T) {
	firstPerson := card("Avery", "Example", []string{"+15555550101"}, nil)
	secondPerson := card("Avery (Jordan)", "Example", []string{"+15555550107"}, nil)

	idx := buildContactPersonIndex([]*imessage.Contact{firstPerson, secondPerson})

	own, other := "tel:+15555550101", "tel:+15555550107"
	if idx.owner[own] == idx.owner[other] {
		t.Fatalf("owner[%s] == owner[%s] (%q) — a parenthetical qualifier marks a DIFFERENT person",
			own, other, idx.owner[own])
	}
	c := bookClient([]*imessage.Contact{firstPerson, secondPerson})
	if got := c.mutualContactHandles(own); got != nil {
		t.Errorf("mutualContactHandles(%s) = %#v, want nil — must not reach the other person's number", own, got)
	}
}

func TestBuildContactPersonIndexIgnoresUnnamedCards(t *testing.T) {
	book := []*imessage.Contact{
		card("Avery", "", []string{"+15555550199"}, nil),
		{Phones: []string{"+15555550199"}}, // unnamed
	}

	idx := buildContactPersonIndex(book)

	if _, amb := idx.ambiguous[sharedLine]; amb {
		t.Error("an unnamed card must not make a handle ambiguous — it drives no merge of its own")
	}
}

// TestMutualContactHandlesRefusesSharedLine is the regression test for the
// reported bug: the person's card lists the shared line, but the line is not
// evidence about her, so it must not become an alternate handle. Before the
// guard this returned the shared line and her whole DM was redirected onto it.
func TestMutualContactHandlesRefusesSharedLine(t *testing.T) {
	c := bookClient(householdBook())

	for _, handle := range []string{parentAMobile, parentBMobile, sharedLine} {
		if got := c.mutualContactHandles(handle); got != nil {
			t.Errorf("mutualContactHandles(%s) = %#v, want nil — its only alternate is shared", handle, got)
		}
	}
}

// Card order must not decide the outcome: whichever card the lookup index would
// return for the shared line, neither may merge through it.
func TestMutualContactHandlesIndependentOfCardOrder(t *testing.T) {
	book := householdBook()
	reversed := []*imessage.Contact{book[2], book[1], book[0]}

	for name, b := range map[string][]*imessage.Contact{"forward": book, "reversed": reversed} {
		t.Run(name, func(t *testing.T) {
			c := bookClient(b)
			for _, handle := range []string{parentAMobile, parentBMobile, sharedLine} {
				if got := c.mutualContactHandles(handle); got != nil {
					t.Errorf("mutualContactHandles(%s) = %#v, want nil", handle, got)
				}
			}
			if got := c.mutualContactHandles(soloMobile); !reflect.DeepEqual(got, []string{soloEmail}) {
				t.Errorf("mutualContactHandles(%s) = %#v, want [%s]", soloMobile, got, soloEmail)
			}
		})
	}
}

// The guard must not break the feature it protects: one person with a phone and
// an email still merges, in both directions.
func TestMutualContactHandlesAllowsGenuineMultiHandleContact(t *testing.T) {
	c := bookClient(householdBook())

	if got := c.mutualContactHandles(soloMobile); !reflect.DeepEqual(got, []string{soloEmail}) {
		t.Errorf("mutualContactHandles(%s) = %#v, want [%s]", soloMobile, got, soloEmail)
	}
	if got := c.mutualContactHandles(soloEmail); !reflect.DeepEqual(got, []string{soloMobile}) {
		t.Errorf("mutualContactHandles(%s) = %#v, want [%s]", soloEmail, got, soloMobile)
	}
}

func TestMutualContactHandlesNoStoreOrNoCard(t *testing.T) {
	if got := testNameClient(nil).mutualContactHandles(parentAMobile); got != nil {
		t.Errorf("mutualContactHandles() with no contact store = %#v, want nil", got)
	}
	c := bookClient(householdBook())
	if got := c.mutualContactHandles("tel:+15555550999"); got != nil {
		t.Errorf("mutualContactHandles() for an unknown handle = %#v, want nil", got)
	}
}

func TestContactHandleOwnerReportsAmbiguity(t *testing.T) {
	c := bookClient(householdBook())

	if owner, amb := c.contactHandleOwner(sharedLine); !amb || owner != "" {
		t.Errorf("contactHandleOwner(%s) = (%q, %v), want ambiguous with no owner", sharedLine, owner, amb)
	}
	owner, amb := c.contactHandleOwner(parentAMobile)
	if amb || owner == "" {
		t.Errorf("contactHandleOwner(%s) = (%q, %v), want an unambiguous owner", parentAMobile, owner, amb)
	}
	if unknown, amb := c.contactHandleOwner("tel:+15555550999"); amb || unknown != "" {
		t.Errorf("contactHandleOwner(unknown) = (%q, %v), want no owner and not ambiguous", unknown, amb)
	}
}

// canonicalContactHandle picks a deterministic canonical for a real multi-handle
// contact and leaves a guard-refused handle alone.
func TestCanonicalContactHandleRespectsGuard(t *testing.T) {
	c := bookClient(householdBook())

	if got := c.canonicalContactHandle(parentAMobile); got != parentAMobile {
		t.Errorf("canonicalContactHandle(%s) = %q, want it unchanged — its only alternate is shared",
			parentAMobile, got)
	}
	// Casey's handles are phone + email; the tel: preference makes the phone
	// canonical from either one.
	if got := c.canonicalContactHandle(soloEmail); got != soloMobile {
		t.Errorf("canonicalContactHandle(%s) = %q, want %q", soloEmail, got, soloMobile)
	}
	if got := c.canonicalContactHandle(soloMobile); got != soloMobile {
		t.Errorf("canonicalContactHandle(%s) = %q, want %q", soloMobile, got, soloMobile)
	}
}

// getContactChatGUIDs must not reach into another person's chats through a
// shared handle, but still covers a genuine contact's alternate handle.
func TestGetContactChatGUIDsRespectsGuard(t *testing.T) {
	c := bookClient(householdBook())

	own := portalIDToChatGUIDs(parentAMobile)
	if got := c.getContactChatGUIDs(parentAMobile); !reflect.DeepEqual(got, own) {
		t.Errorf("getContactChatGUIDs(%s) = %#v, want only its own GUIDs %#v", parentAMobile, got, own)
	}
	if got := c.getContactChatGUIDs(soloMobile); len(got) <= len(portalIDToChatGUIDs(soloMobile)) {
		t.Errorf("getContactChatGUIDs(%s) = %#v, want the email handle's GUIDs included too", soloMobile, got)
	}
}

// countNonSelfMembers must not collapse two people through a shared handle: a
// two-person group counted as one member is routed as a DM.
func TestCountNonSelfMembersDoesNotCollapseSharedHandle(t *testing.T) {
	c := bookClient(householdBook())

	if got := c.countNonSelfMembers([]string{parentAMobile, sharedLine}, nil); got != 2 {
		t.Errorf("countNonSelfMembers(parent, shared line) = %d, want 2 — they are not provably one person", got)
	}
	// One person's own two handles still collapse to one member.
	if got := c.countNonSelfMembers([]string{soloMobile, soloEmail}, nil); got != 1 {
		t.Errorf("countNonSelfMembers(solo phone, solo email) = %d, want 1", got)
	}
}

// The index is cached per contact-cache generation: a sync that adds the second
// claimant to a handle must invalidate it, or the guard would keep answering
// from an address book that no longer exists.
func TestContactPersonIndexRebuildsOnCacheGenerationChange(t *testing.T) {
	solo := card("Avery", "Example", []string{"+15555550101", "+15555550199"}, nil)
	store := &fakeContactSource{
		contacts: map[string]*imessage.Contact{"+15555550101": solo, "+15555550199": solo},
		count:    1,
		lastSync: time.Now(),
	}
	c := testNameClient(store)

	// Avery alone lists the line, so it is theirs and merges — the behaviour
	// before any overlap exists.
	if got := c.mutualContactHandles(parentAMobile); !reflect.DeepEqual(got, []string{sharedLine}) {
		t.Fatalf("mutualContactHandles(%s) = %#v, want [%s] while only one person claims the line",
			parentAMobile, got, sharedLine)
	}

	// A later sync adds the second person, who also lists the line.
	blakeCard := card("Blake", "Example", []string{"+15555550102", "+15555550199"}, nil)
	store.contacts = map[string]*imessage.Contact{
		"+15555550101": solo, "+15555550102": blakeCard, "+15555550199": solo,
	}
	store.count = 2
	store.lastSync = store.lastSync.Add(time.Minute)

	if _, amb := c.contactPersonIndex().ambiguous[sharedLine]; !amb {
		t.Errorf("ambiguous[%s] missing after the sync that added the second claimant — cache not invalidated", sharedLine)
	}
	if got := c.mutualContactHandles(parentAMobile); got != nil {
		t.Errorf("mutualContactHandles(%s) = %#v, want nil once the line is shared", parentAMobile, got)
	}
}

// TestMutualContactHandlesPutsPhonesFirst pins the ordering regression found on
// a live bridge: resolveContactPortalID adopts the first alternate that already
// has a room, so a plain lexicographic sort ("mailto:" < "tel:") moved every
// multi-handle contact's DM from their phone room into an email room.
func TestMutualContactHandlesPutsPhonesFirst(t *testing.T) {
	dana := &imessage.Contact{
		FirstName: "Dana", LastName: "Example",
		Phones: []string{"+15555550105"},
		Emails: []string{"d.example@icloud.example", "dana@example.net"},
	}
	c := bookClient([]*imessage.Contact{dana})

	got := c.mutualContactHandles("mailto:dana@example.net")
	want := []string{
		"tel:+15555550105",
		"mailto:d.example@icloud.example",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mutualContactHandles() = %#v, want %#v — the tel: handle must be offered first",
			got, want)
	}
	// From the phone handle the emails stay in a stable, deterministic order.
	got = c.mutualContactHandles("tel:+15555550105")
	want = []string{
		"mailto:d.example@icloud.example",
		"mailto:dana@example.net",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mutualContactHandles(phone) = %#v, want %#v", got, want)
	}
}

func TestSortContactHandles(t *testing.T) {
	got := sortContactHandles([]string{
		"mailto:z@example.com", "tel:+15555550102", "mailto:a@example.com", "tel:+15555550101",
	})
	want := []string{
		"tel:+15555550101", "tel:+15555550102", "mailto:a@example.com", "mailto:z@example.com",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortContactHandles() = %#v, want %#v", got, want)
	}
}

// TestBuildContactPersonIndexKeepsSameNamedStrangersApart is the reviewer's
// reproduction from PR #31: two different people who happen to share a name,
// each with their own handle and nothing in common.
//
// Keying a person on the name alone unioned their handle sets, so
// resolveContactPortalID would adopt the stranger's portal, canonicalizeDMSender
// would relabel the inbound messages to match, and resolveSendTarget would fall
// back to the stranger's number — this file's own bug via a different route. It
// was also a regression: before the person index, resolveContactPortalID read a
// single card, so two one-handle cards hit len(altIDs) <= 1 and never merged.
func TestBuildContactPersonIndexKeepsSameNamedStrangersApart(t *testing.T) {
	one := card("Jordan", "Example", []string{"+15551111111"}, nil)
	two := card("Jordan", "Example", []string{"+15552222222"}, nil)

	idx := buildContactPersonIndex([]*imessage.Contact{one, two})

	a, b := "tel:+15551111111", "tel:+15552222222"
	if idx.owner[a] == idx.owner[b] {
		t.Fatalf("owner[%s] == owner[%s] (%q) — an identical name is not proof of one person", a, b, idx.owner[a])
	}
	c := bookClient([]*imessage.Contact{one, two})
	if got := c.mutualContactHandles(a); got != nil {
		t.Errorf("mutualContactHandles(%s) = %#v, want nil — must not reach the stranger's number", a, got)
	}
	if got := c.mutualContactHandles(b); got != nil {
		t.Errorf("mutualContactHandles(%s) = %#v, want nil", b, got)
	}
}

// The other half, which had no coverage at all: one person split across two
// cards that DO corroborate each other by sharing a handle. Their handles must
// still union, or the cross-card merge this index exists for is dead.
func TestBuildContactPersonIndexUnionsCorroboratedCards(t *testing.T) {
	// Both cards carry the mobile; each adds an address the other lacks.
	first := card("Riley", "Example", []string{"+15555550110"}, []string{"riley@example.com"})
	second := card("Riley", "Example", []string{"+15555550110"}, []string{"riley.example@icloud.example"})

	idx := buildContactPersonIndex([]*imessage.Contact{first, second})

	phone := "tel:+15555550110"
	if _, amb := idx.ambiguous[phone]; amb {
		t.Fatalf("ambiguous[%s] set — a shared handle is what proves the two cards are one person", phone)
	}
	got := idx.handles[idx.owner[phone]]
	want := []string{phone, "mailto:riley.example@icloud.example", "mailto:riley@example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("handles[person] = %#v, want %#v (deduped, tel: first)", got, want)
	}
	c := bookClient([]*imessage.Contact{first, second})
	if got := c.mutualContactHandles("mailto:riley@example.com"); len(got) != 2 || got[0] != phone {
		t.Errorf("mutualContactHandles(email) = %#v, want the phone first plus the other email", got)
	}
}

// Three cards chained by shared handles are one person even though no single
// pair carries every handle — the union must be transitive within a name.
func TestBuildContactPersonIndexUnionsTransitivelyWithinAName(t *testing.T) {
	a := card("Sam", "Example", []string{"+15555550120"}, nil)
	b := card("Sam", "Example", []string{"+15555550120", "+15555550121"}, nil)
	cc := card("Sam", "Example", []string{"+15555550121"}, []string{"sam@example.com"})

	idx := buildContactPersonIndex([]*imessage.Contact{a, b, cc})

	owner := idx.owner["tel:+15555550120"]
	if owner == "" || idx.owner["mailto:sam@example.com"] != owner {
		t.Fatalf("the chain a-b-c should be one person: owner[first]=%q owner[last]=%q",
			owner, idx.owner["mailto:sam@example.com"])
	}
	if got := len(idx.handles[owner]); got != 3 {
		t.Errorf("handles[person] has %d entries, want 3 deduped", got)
	}
}

// A same-name stranger must not become ambiguous either — that would refuse the
// merges within each person's own card, which is not what the collision implies.
func TestBuildContactPersonIndexSameNamedStrangersStillMergeTheirOwnHandles(t *testing.T) {
	one := card("Alex", "Example", []string{"+15555550130"}, []string{"alex.one@example.com"})
	two := card("Alex", "Example", []string{"+15555550131"}, []string{"alex.two@example.com"})

	c := bookClient([]*imessage.Contact{one, two})

	if got := c.mutualContactHandles("tel:+15555550130"); !reflect.DeepEqual(got, []string{"mailto:alex.one@example.com"}) {
		t.Errorf("mutualContactHandles() = %#v, want only this card's own email", got)
	}
	if got := c.mutualContactHandles("tel:+15555550131"); !reflect.DeepEqual(got, []string{"mailto:alex.two@example.com"}) {
		t.Errorf("mutualContactHandles() = %#v, want only the other card's own email", got)
	}
}

// TestBuildContactPersonIndexUnionsSameNamedPeopleSharingAHandle pins the known
// limit of the corroboration rule, so a future change to it is a deliberate one.
//
// Two DIFFERENT people who share both a name and a handle still union, and the
// shared handle is NOT marked ambiguous — the household shape with a name
// collision on top. No address book distinguishes that from one person listed
// on two cards, which is the same reason a corroborated union is trusted at all.
// This documents the behaviour rather than endorsing it.
func TestBuildContactPersonIndexUnionsSameNamedPeopleSharingAHandle(t *testing.T) {
	// Two people called Ann Example who both list the household landline.
	annA := card("Ann", "Example", []string{"+15555550140", "+15555550199"}, nil)
	annB := card("Ann", "Example", []string{"+15555550141", "+15555550199"}, nil)

	idx := buildContactPersonIndex([]*imessage.Contact{annA, annB})

	if _, amb := idx.ambiguous[sharedLine]; amb {
		t.Fatalf("ambiguous[%s] set — known limit: a same-name pair corroborates through it", sharedLine)
	}
	if got := len(idx.handles[idx.owner[sharedLine]]); got != 3 {
		t.Errorf("handles[person] = %d, want all 3 unioned (the documented limit)", got)
	}
}

// ...but a third, differently-named person claiming the same handle still makes
// it ambiguous, and that must not cost the same-named pair their own merge.
// This is the case the reviewer most expected to break.
func TestBuildContactPersonIndexAmbiguityBeatsCorroboration(t *testing.T) {
	annA := card("Ann", "Example", []string{"+15555550140", "+15555550199"}, nil)
	annB := card("Ann", "Example", []string{"+15555550141", "+15555550199"}, nil)
	other := card("Blake", "Example", []string{"+15555550142", "+15555550199"}, nil)

	c := bookClient([]*imessage.Contact{annA, annB, other})
	idx := c.contactPersonIndex()

	if _, amb := idx.ambiguous[sharedLine]; !amb {
		t.Fatalf("ambiguous[%s] missing — two named people claim it, so it is evidence about neither", sharedLine)
	}
	// The Anns keep their own handles despite the refused shared line.
	if got := c.mutualContactHandles("tel:+15555550140"); !reflect.DeepEqual(got, []string{"tel:+15555550141"}) {
		t.Errorf("mutualContactHandles(annA) = %#v, want the other Ann handle — refusing the shared line must not cost this", got)
	}
	if got := c.mutualContactHandles("tel:+15555550142"); got != nil {
		t.Errorf("mutualContactHandles(other) = %#v, want nil — the shared line was their only alternate", got)
	}
	if got := c.mutualContactHandles(sharedLine); got != nil {
		t.Errorf("mutualContactHandles(shared) = %#v, want nil", got)
	}
}
