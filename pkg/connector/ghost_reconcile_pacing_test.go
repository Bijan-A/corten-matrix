// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for the ghost profile reconcile pacing predicate.

package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func pacingGhost(name string, nameSet bool, avatarID string, avatarSet bool) *bridgev2.Ghost {
	return &bridgev2.Ghost{Ghost: &database.Ghost{
		Name: name, NameSet: nameSet,
		AvatarID: networkid.AvatarID(avatarID), AvatarSet: avatarSet,
	}}
}
func pacingPtr(s string) *string { return &s }

func TestGhostProfileWouldChange(t *testing.T) {
	cases := []struct {
		name  string
		ghost *bridgev2.Ghost
		info  *bridgev2.UserInfo
		want  bool
	}{
		{"settled name and avatar, nothing to write", pacingGhost("Sam", true, "a1", true),
			&bridgev2.UserInfo{Name: pacingPtr("Sam"), Avatar: &bridgev2.Avatar{ID: "a1"}}, false},
		{"name differs", pacingGhost("Sam", true, "a1", true),
			&bridgev2.UserInfo{Name: pacingPtr("Samantha"), Avatar: &bridgev2.Avatar{ID: "a1"}}, true},
		{"avatar differs", pacingGhost("Sam", true, "a1", true),
			&bridgev2.UserInfo{Name: pacingPtr("Sam"), Avatar: &bridgev2.Avatar{ID: "a2"}}, true},
		{"same name but never pushed", pacingGhost("Sam", false, "a1", true),
			&bridgev2.UserInfo{Name: pacingPtr("Sam")}, true},
		{"same avatar but never pushed", pacingGhost("Sam", true, "a1", false),
			&bridgev2.UserInfo{Avatar: &bridgev2.Avatar{ID: "a1"}}, true},
		{"info carries neither", pacingGhost("Sam", true, "a1", true), &bridgev2.UserInfo{}, false},
		{"nil info", pacingGhost("Sam", true, "a1", true), nil, false},
		{"nil ghost", nil, &bridgev2.UserInfo{Name: pacingPtr("Sam")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ghostProfileWouldChange(tc.ghost, tc.info); got != tc.want {
				t.Errorf("ghostProfileWouldChange() = %v, want %v", got, tc.want)
			}
		})
	}
}
