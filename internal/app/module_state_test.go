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

func TestModuleStatusNotice(t *testing.T) {
	mail, _ := moduleByID("mail")
	if n := mail.statusFor(scopeSet(strings.Join(eveScopes, " "))).notice(mail, "Pilot"); n != nil {
		t.Errorf("enabled module produced a notice: %+v", n)
	}
	locked := mail.statusFor(scopeSet("")).notice(mail, "Pilot")
	if locked == nil || !locked.Locked || len(locked.Scopes) != 1 {
		t.Fatalf("locked notice = %+v, want Locked with the one required scope", locked)
	}
	limited := mail.statusFor(scopeSet("esi-mail.read_mail.v1")).notice(mail, "Pilot")
	if limited == nil || limited.Locked || len(limited.Scopes) != 2 {
		t.Fatalf("limited notice = %+v, want not Locked with two optional scopes", limited)
	}
}

func TestLockedNoticeTemplate(t *testing.T) {
	mail, _ := moduleByID("mail")
	ts, err := parsedTemplate(&fragmentTemplates, "fragment", "locked.html", "templates/locked.html")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	n := mail.statusFor(scopeSet("")).notice(mail, "Pilot One")
	if err := ts.ExecuteTemplate(&b, "locked-notice", n); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"Mail is locked", "Inbox, labels, mailing lists and bodies.", "Pilot One", `href="/auth/eve"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered notice missing %q:\n%s", want, out)
		}
	}
	b.Reset()
	if err := ts.ExecuteTemplate(&b, "locked-notice", (*lockedNotice)(nil)); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(b.String()) != "" {
		t.Errorf("nil notice rendered %q, want nothing", b.String())
	}
}
