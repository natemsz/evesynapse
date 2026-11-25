package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestHomeWidgetRouteServesOnlyTheAccountsOwnCards: the one-card route
// draws a card from the signed-in account's own data only. Anonymous
// requests are refused, another account's characters never appear, and
// a widget that is not on the account's home is not served.
func TestHomeWidgetRouteServesOnlyTheAccountsOwnCards(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	alpha, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	bravo, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, alpha.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, bravo.ID, fixtureCharB, "Fixture Bravo")
	setHomeLayout(t, q, alpha.ID, `[{"id":"fleet"}]`)
	setHomeLayout(t, q, bravo.ID, `[{"id":"fleet"}]`)
	cookie := sessionCookie(t, app, bravo.ID, fixtureCharB, "Fixture Bravo")

	code, body := getPage(t, app, cookie, "/home/widget/fleet")
	if code != http.StatusOK {
		t.Fatalf("GET own card: status %d", code)
	}
	mustContain(t, "/home/widget/fleet", body, `data-widget="fleet"`, "Fixture Bravo")
	if strings.Contains(body, "Fixture Alpha") {
		t.Error("the card drew another account's character")
	}
	if code, _ := getPage(t, app, cookie, "/home/widget/networth"); code != http.StatusNotFound {
		t.Errorf("GET a card that is not on the home: status %d, want 404", code)
	}
	if code, body, _ := doReq(t, app, http.MethodGet, "/home/widget/fleet", nil); code == http.StatusOK || strings.Contains(body, "Fixture") {
		t.Errorf("anonymous GET: status %d, want refused with no card", code)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}
