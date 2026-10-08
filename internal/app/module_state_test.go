package app

import (
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

func TestModuleStatusFor(t *testing.T) {
	mail, _ := moduleByID("mail")
	cases := []struct {
		name    string
		granted []string
		state   moduleState
		missReq int
		missOpt int
	}{
		{"none", nil, moduleLocked, 1, 2},
		{"required only", []string{"esi-mail.read_mail.v1"}, moduleLimited, 0, 2},
		{"one optional", []string{"esi-mail.read_mail.v1", mailSendScope}, moduleLimited, 0, 1},
		{"all", []string{"esi-mail.read_mail.v1", mailSendScope, mailOrganizeScope}, moduleEnabled, 0, 0},
		{"optional without required", []string{mailSendScope, mailOrganizeScope}, moduleLocked, 1, 0},
	}
	for _, c := range cases {
		st := mail.statusFor(scopeSet(strings.Join(c.granted, " ")))
		if st.State != c.state || len(st.MissingRequired) != c.missReq || len(st.MissingOptional) != c.missOpt {
			t.Errorf("%s: got %s req=%d opt=%d, want %s req=%d opt=%d", c.name,
				st.State, len(st.MissingRequired), len(st.MissingOptional), c.state, c.missReq, c.missOpt)
		}
	}
}

func TestPublicAndDerivedModulesAreAlwaysEnabled(t *testing.T) {
	states := characterModuleStates(db.Character{})
	for _, m := range moduleManifest {
		if m.Layer == layerPublic || m.Layer == layerDerived {
			if states[m.ID].State != moduleEnabled {
				t.Errorf("%s module %q = %s, want enabled", m.Layer, m.ID, states[m.ID].State)
			}
		}
	}
}

func TestCharacterModuleStatesFromGrantedScopes(t *testing.T) {
	ch := db.Character{Scopes: strings.Join(eveScopes, " ")}
	for id, st := range characterModuleStates(ch) {
		if st.State != moduleEnabled {
			t.Errorf("full grant: module %q = %s", id, st.State)
		}
	}
	none := characterModuleStates(db.Character{})
	if none["skills"].State != moduleLocked {
		t.Errorf("no grant: skills = %s, want locked", none["skills"].State)
	}
}
