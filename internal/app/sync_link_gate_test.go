package app

import (
	"context"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The Sync page is admin-only, so a /sync/ link anywhere else is a
// dead end for every other signed-in user: every href="/sync/" in a
// template must sit inside the true branch of an {{if .IsAdmin}}.
// admin.html and sync.html are exempt — both pages are themselves
// behind requireAdmin, so their links cannot dead-end.
func TestSyncLinksAreAdminGated(t *testing.T) {
	entries, err := fs.ReadDir(templatesFS, "templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "admin.html" || name == "sync.html" {
			continue
		}
		raw, err := fs.ReadFile(templatesFS, "templates/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if bad := ungatedSyncLinks(string(raw)); len(bad) > 0 {
			t.Errorf("%s: /sync/ link outside an admin gate: %s", name, bad[0])
		}
	}
}

var templateActionRe = regexp.MustCompile(`\{\{-?\s*(if|range|with|define|block|else|end)\b([^{}]*?)-?\}\}`)

// ungatedSyncLinks walks a template's action stack and returns the
// href="/sync/" occurrences that no enclosing admin gate covers. A
// gate covers an anchor only if it can actually read the flag where
// it sits: an {{if}} inside a {{with}}/{{range}} evaluates against
// the rebound dot, so there the gate must go through the root
// ($.IsAdmin); a gate outside the rebound evaluates against the
// page root, where .IsAdmin is the shell's flag.
func ungatedSyncLinks(src string) []string {
	// Template comments can hold braces and links; they never render.
	for {
		i := strings.Index(src, "{{/*")
		if i < 0 {
			break
		}
		j := strings.Index(src[i:], "*/}}")
		if j < 0 {
			break
		}
		src = src[:i] + src[i+j+4:]
	}
	type frame struct {
		rebinds bool // with/range: actions below evaluate against a new dot
		gate    bool // if-frame in a branch whose condition checks the admin flag
		rooted  bool // the condition reads the flag through the root ($.IsAdmin)
	}
	gated := func(stack []frame) bool {
		for i := len(stack) - 1; i >= 0; i-- {
			if !stack[i].gate {
				continue
			}
			rebound := false
			for j := 0; j < i; j++ {
				if stack[j].rebinds {
					rebound = true
					break
				}
			}
			if !rebound || stack[i].rooted {
				return true
			}
		}
		return false
	}
	var bad []string
	var stack []frame
	rest := src
	for {
		m := templateActionRe.FindStringSubmatchIndex(rest)
		segEnd := len(rest)
		if m != nil {
			segEnd = m[0]
		}
		if seg := rest[:segEnd]; strings.Contains(seg, `href="/sync/"`) && !gated(stack) {
			bad = append(bad, strings.TrimSpace(seg[max(0, len(seg)-80):]))
		}
		if m == nil {
			break
		}
		kw := rest[m[2]:m[3]]
		cond := ""
		if m[4] >= 0 {
			cond = rest[m[4]:m[5]]
		}
		switch kw {
		case "if":
			stack = append(stack, frame{
				gate:   strings.Contains(cond, ".IsAdmin"),
				rooted: strings.Contains(cond, "$.IsAdmin"),
			})
		case "range", "with":
			stack = append(stack, frame{rebinds: true})
		case "define", "block":
			stack = append(stack, frame{})
		case "else":
			if len(stack) > 0 {
				// An else branch keeps a gate only when the else-if
				// condition is itself the admin check.
				top := &stack[len(stack)-1]
				top.gate = strings.Contains(cond, "if") && strings.Contains(cond, ".IsAdmin")
				top.rooted = strings.Contains(cond, "$.IsAdmin")
			}
		case "end":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
		rest = rest[m[1]:]
	}
	return bad
}

// A warming page shows the Sync-page direction only to admins:
// everyone else gets the plain sentence and no /sync/ link anywhere.
func TestWarmingSyncLinkFollowsAdmin(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Second Pilot")
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (34, 'Tritanium', 18)`,
		`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita 4 - Moon 4 - Caldari Navy Assembly Plant', 30000142)`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.9)`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	code, body := getPage(t, app, cookie, "/industry/?character=90000002")
	if code != 200 {
		t.Fatalf("industry (B): status %d", code)
	}
	if !strings.Contains(body, "Still syncing") {
		t.Fatal("non-admin warming page lost the syncing sentence")
	}
	if strings.Contains(body, `href="/sync/"`) {
		t.Fatal("non-admin warming page links to the admin-only Sync page")
	}

	grantTestAdmin(app, fixtureCharA)
	code, body = getPage(t, app, cookie, "/industry/?character=90000002")
	if code != 200 {
		t.Fatalf("industry (B) as admin: status %d", code)
	}
	if !strings.Contains(body, `Still syncing. Progress is on the <a href="/sync/">Sync page</a>.`) {
		t.Fatal("admin warming page lost the Sync page link")
	}
}
