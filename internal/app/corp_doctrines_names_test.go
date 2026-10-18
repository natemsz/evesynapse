package app

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestDoctrineFitBrowserSearchesItemNames: the corporate fits
// browser finds a fit by the names of the modules it carries —
// the search text behind each card is built from the names
// resolved for the listed fits (items and loaded-charge weapons
// alike).
func TestDoctrineFitBrowserSearchesItemNames(t *testing.T) {
	f := newNotifyFixture(t)
	director := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")

	if _, err := f.app.db.ExecContext(f.ctx, `INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
		(2048, 'Test Module XL', 400, 0, 1), (4242, 'Test Charge S', 400, 0, 1)`); err != nil {
		t.Fatalf("seed type names: %v", err)
	}
	now := time.Now().UTC()
	fit, err := f.q.CreateLocalFitting(f.ctx, db.CreateLocalFittingParams{
		UserID: f.userID, Name: "Named Fit", ShipTypeID: 587, IsPublic: false, CreatedAt: now, UpdatedAt: now,
		ItemsJson: `{"name":"Named Fit","tags":[],"shipTypeId":587,"items":[{"typeId":2048,"qty":1}],"charges":{"2048":4242}}`,
	})
	if err != nil {
		t.Fatalf("seed fit: %v", err)
	}
	f.post(director, "/doctrines/create", url.Values{"corporation": {"98000001"}, "name": {"Named Doctrine"}})
	made := f.doctrines(discordCorp)
	if len(made) != 1 {
		t.Fatalf("doctrines %+v, want the one made", made)
	}
	f.post(director, doctrineURL(made[0].ID)+"/fits/add", url.Values{"fit": {strconv.FormatInt(fit.ID, 10)}})

	_, body := getPage(t, f.app, director, doctrineFitsPath+"?q=test+module")
	mustContain(t, "corporate fits by module name", body, "Named Fit")
	_, body = getPage(t, f.app, director, doctrineFitsPath+"?q=zzz-no-match")
	if strings.Contains(body, "Named Fit") {
		t.Fatal("a fit matched a search none of its names contain")
	}
}
