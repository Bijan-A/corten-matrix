// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for the cross-portal backfill duplicate guard.

package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func crossPortalPart(portalID string) *database.Message {
	return &database.Message{Room: networkid.PortalKey{ID: networkid.PortalID(portalID)}}
}

// TestMessagePartsInRoom is the predicate the duplicate guard turns on.
//
// The distinction is the whole fix: a row for this GUID in ANOTHER portal means
// the insert here will collide on the room-agnostic message_real_pkey, and
// since bridgev2 sends before it saves, sending would post a duplicate that
// cannot be persisted — again on every restart. A row in THIS portal is the
// ordinary already-delivered case, owned by the existing skip/reconcile logic,
// and must NOT be swept up by this guard.
func TestMessagePartsInRoom(t *testing.T) {
	const here = networkid.PortalID("tel:+15551230000")
	cases := []struct {
		name  string
		parts []*database.Message
		want  bool
	}{
		{"no parts at all — never bridged, send it", nil, false},
		{
			// The reported bug: same conversation, two portals.
			name:  "only in another portal — the collision case",
			parts: []*database.Message{crossPortalPart("mailto:sam@example.com")},
			want:  false,
		},
		{
			name:  "already in this portal — not ours to suppress",
			parts: []*database.Message{crossPortalPart(string(here))},
			want:  true,
		},
		{
			// A multi-part message can straddle both; presence here wins, so
			// the guard stays off and existing dedup decides.
			name:  "multi-part with one part here",
			parts: []*database.Message{crossPortalPart("mailto:sam@example.com"), crossPortalPart(string(here))},
			want:  true,
		},
		{
			name:  "nil part is skipped, not treated as a match",
			parts: []*database.Message{nil, crossPortalPart("mailto:sam@example.com")},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := messagePartsInRoom(tc.parts, here); got != tc.want {
				t.Errorf("messagePartsInRoom() = %v, want %v", got, tc.want)
			}
		})
	}
}
