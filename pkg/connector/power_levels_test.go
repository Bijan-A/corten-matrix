// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for the portal power-level overrides.

package connector

import (
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	plTestBot   = id.UserID("@imessagebot:example.com")
	plTestUser  = id.UserID("@user:example.com")
	plTestAdmin = id.UserID("@admin:example.com")
)

func assertThresholdsLocked(t *testing.T, pl *event.PowerLevelsEventContent) {
	t.Helper()
	for name, got := range map[string]int{
		"invite":     pl.Invite(),
		"kick":       pl.Kick(),
		"ban":        pl.Ban(),
		"redact":     pl.Redact(),
		"tombstone":  pl.GetEventLevel(event.StateTombstone),
		"encryption": pl.GetEventLevel(event.StateEncryption),
		"server_acl": pl.GetEventLevel(event.StateServerACL),
	} {
		if got != plLockLevel {
			t.Errorf("%s level = %d, want %d", name, got, plLockLevel)
		}
	}
	if pl.EventsDefault != 0 {
		t.Errorf("events_default = %d, want 0 so ghosts and users can always send", pl.EventsDefault)
	}
}

// TestHardenedPowerLevelsLeaveUserLevelsAlone pins that a level granted with
// `set-pl` survives the overrides. They are re-applied on every member resync,
// and they used to reset every non-bot user to users_default, which made set-pl
// impossible to use: the level snapped back the moment it was set.
func TestHardenedPowerLevelsLeaveUserLevelsAlone(t *testing.T) {
	// A room as bridgev2 creates it, after an admin granted two users levels:
	// one ordinary, one high enough for the locked actions.
	pl := &event.PowerLevelsEventContent{
		Users: map[id.UserID]int{
			plTestBot:   botPowerLevel,
			plTestUser:  100,
			plTestAdmin: plLockLevel,
		},
	}

	if !hardenedPowerLevels().Apply(plTestBot, pl) {
		t.Fatal("first apply to a room at default thresholds reported no change")
	}
	if got := pl.GetUserLevel(plTestUser); got != 100 {
		t.Errorf("granted user's level = %d after apply, want 100 — set-pl must stick", got)
	}
	if got := pl.GetUserLevel(plTestAdmin); got != plLockLevel {
		t.Errorf("granted admin's level = %d after apply, want %d", got, plLockLevel)
	}
	if got := pl.GetUserLevel(plTestBot); got != botPowerLevel {
		t.Errorf("bot level = %d, want %d", got, botPowerLevel)
	}
	assertThresholdsLocked(t, pl)

	// A second apply must report no change. HandleMatrixPowerLevels re-sends
	// only when Apply changed something, so a no-op here is what lets a user's
	// level edit stand without a snap-back.
	if hardenedPowerLevels().Apply(plTestBot, pl) {
		t.Error("re-applying to a room already within policy reported a change; granted levels would be re-sent on every edit")
	}
}

// TestHardenedPowerLevelsRestoreLoweredThresholds pins the half of the policy
// that stays: a user with enough power to edit levels can still raise others,
// but a threshold they lower is restored, while the user levels in the same
// edit are kept.
func TestHardenedPowerLevelsRestoreLoweredThresholds(t *testing.T) {
	pl := &event.PowerLevelsEventContent{
		Users: map[id.UserID]int{
			plTestBot:   botPowerLevel,
			plTestAdmin: plLockLevel,
			plTestUser:  50,
		},
		EventsDefault: 50,
		Events: map[string]int{
			event.StateTombstone.Type: 0,
		},
	}
	pl.InvitePtr = new(int) // lowered to 0

	if !hardenedPowerLevels().Apply(plTestBot, pl) {
		t.Fatal("apply did not restore lowered thresholds")
	}
	assertThresholdsLocked(t, pl)
	if got := pl.GetUserLevel(plTestUser); got != 50 {
		t.Errorf("user level set in the same edit = %d, want 50", got)
	}
	if got := pl.GetUserLevel(plTestAdmin); got != plLockLevel {
		t.Errorf("admin level = %d, want %d", got, plLockLevel)
	}
}
