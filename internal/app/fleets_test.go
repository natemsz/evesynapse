package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestAnonymousViewerGetsNoDetailsOfARestrictedFleet: without standing,
// a private fleet answers the same as one that does not exist, by its
// address, its share link and its calendar file. A link-only fleet
// opens by its link, with no pilot names and no form-up.
func TestAnonymousViewerGetsNoDetailsOfARestrictedFleet(t *testing.T) {
	f := newOpsFixture(t)
	id := f.create(time.Now().UTC().Add(48*time.Hour).Truncate(time.Minute), "Secret strat op")
	op, err := f.q.GetOp(f.ctx, id)
	if err != nil || !op.ShareToken.Valid {
		t.Fatalf("stored fleet: %+v (%v)", op, err)
	}
	link := "/f/" + op.ShareToken.String + "/"
	anonymous := func(path string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		f.app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	secrets := []string{"Secret strat op", "Jita 4-4", "Bring probes.", "Shield Lokis", "Fixture Ceo"}

	missingCode, missing := anonymous("/f/no-such-link/")
	if missingCode != http.StatusNotFound || !strings.Contains(missing, "Fleet not available") {
		t.Fatalf("a link that does not exist: %d", missingCode)
	}
	for _, path := range []string{opURL(id), opURL(id) + "edit", link} {
		code, body := anonymous(path)
		if code != missingCode || !strings.Contains(body, "Fleet not available") {
			t.Errorf("GET %s: %d, want the answer for a fleet that does not exist", path, code)
		}
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s shows %q", path, secret)
			}
		}
	}
	if code, _ := anonymous(link + "fleet.ics"); code != http.StatusNotFound {
		t.Errorf("the calendar file of a private fleet: %d", code)
	}
	if _, body := anonymous("/fleets/"); strings.Contains(body, "Secret strat op") {
		t.Error("the board lists a private fleet")
	}

	if err := f.q.UpdateFleetAccess(f.ctx, db.UpdateFleetAccessParams{
		ID: id, FleetType: op.FleetType, Visibility: fleetUnlisted, JoinGate: joinByRule,
	}); err != nil {
		t.Fatal(err)
	}
	code, body := anonymous(link)
	if code != http.StatusOK || !strings.Contains(body, "Secret strat op") {
		t.Fatalf("a link-only fleet by its link: %d", code)
	}
	for _, secret := range []string{"Jita 4-4", "Fixture Ceo"} {
		if strings.Contains(body, secret) {
			t.Errorf("the share page shows %q to an anonymous viewer", secret)
		}
	}
	if code, _ := anonymous(opURL(id)); code != http.StatusNotFound {
		t.Errorf("a link-only fleet by its address, with no link: %d", code)
	}
}
