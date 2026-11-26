package tests

// My Loadouts unification + public-fit owner editing: the saved
// page lists the user's EveSynapse fits beside the character's
// EVE fittings with a source filter, and a public fit opened by
// its owner loads as their saved fit (edits land in place),
// while another user's public fit still opens unsaved (a copy).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/apptest"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func seedLocalFit(t *testing.T, q *db.Queries, userID int64, name string, isPublic, isDraft bool) db.CreateLocalFittingRow {
	t.Helper()
	doc := fmt.Sprintf(`{"name":%q,"shipTypeId":587,"items":[{"typeId":3244,"qty":1}],"charges":{}}`, name)
	row, err := q.CreateLocalFitting(context.Background(), db.CreateLocalFittingParams{
		UserID: userID, Name: name, ShipTypeID: 587, ItemsJson: doc,
		IsPublic: isPublic, IsDraft: isDraft,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create local fit %q: %v", name, err)
	}
	return row
}

func TestMyLoadoutsSourcesAndOwnerEdit(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	conn := rig.DB()
	q := rig.Queries()
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (3244, 'Medium Shield Extender I', 38)`); err != nil {
		t.Fatalf("seed sde: %v", err)
	}
	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapFittings, esi.Fittings{
		{FittingID: 42, Name: "Alpha Rifter", ShipTypeID: 587,
			Items: []esi.FittingItem{{TypeID: 3244, Quantity: 1, Flag: "MedSlot0"}}},
	})

	publicFit := seedLocalFit(t, q, user.ID, "Harbor Public Rifter", true, false)
	seedLocalFit(t, q, user.ID, "Harbor Private Rifter", false, false)
	seedLocalFit(t, q, user.ID, "Harbor Draft Rifter", false, true)
	strangerFit := seedLocalFit(t, q, other.ID, "Stranger Public Rifter", true, false)

	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Alpha Pilot")

	// All: every source together, drafts excluded, chips counted.
	_, body := apptest.GetPage(t, rig, cookie, "/fittings/saved/")
	apptest.MustContain(t, "/fittings/saved/", body,
		"Harbor Public Rifter", "Harbor Private Rifter", "Alpha Rifter",
		`<span class="l-stat-label">Public</span><span class="l-stat-value">1</span>`, `<span class="l-stat-label">EveSynapse</span><span class="l-stat-value">2</span>`, `<span class="l-stat-label">EVE account</span><span class="l-stat-value">1</span>`,
		fmt.Sprintf("/fittings/?local=%d#fit-editor", publicFit.ID),
		fmt.Sprintf("/fittings/public/fit/%d/", publicFit.ID),
		// The training-plan glyph rides on both fit sources.
		fmt.Sprintf("/skills/plans/fit?character=90000001&amp;local=%d", publicFit.ID),
		"/skills/plans/fit?character=90000001&amp;fitting=42",
		// The source tiles keep the character as its own parameter.
		`href="/fittings/saved/?source=eve&character=90000001"`,
	)
	if strings.Contains(body, "Harbor Draft Rifter") {
		t.Errorf("saved page lists the autosave draft")
	}

	// The source filters narrow to exactly their bucket.
	_, body = apptest.GetPage(t, rig, cookie, "/fittings/saved/?source=public")
	apptest.MustContain(t, "source=public", body, "Harbor Public Rifter")
	if strings.Contains(body, "Harbor Private Rifter") || strings.Contains(body, "Alpha Rifter") {
		t.Errorf("source=public leaked another bucket")
	}
	_, body = apptest.GetPage(t, rig, cookie, "/fittings/saved/?source=evesynapse")
	apptest.MustContain(t, "source=evesynapse", body, "Harbor Public Rifter", "Harbor Private Rifter")
	if strings.Contains(body, "Alpha Rifter") {
		t.Errorf("source=evesynapse leaked the EVE fit")
	}
	_, body = apptest.GetPage(t, rig, cookie, "/fittings/saved/?source=eve")
	apptest.MustContain(t, "source=eve", body, "Alpha Rifter")
	if strings.Contains(body, "Harbor Public Rifter") || strings.Contains(body, "Harbor Private Rifter") {
		t.Errorf("source=eve leaked a local fit")
	}

	// The owner opens their own public fit as their saved fit:
	// the editor carries its id, so saving edits it in place.
	_, body = apptest.GetPage(t, rig, cookie, fmt.Sprintf("/fittings/?public=%d", publicFit.ID))
	apptest.MustContain(t, "own public fit in the editor", body,
		fmt.Sprintf(`id="fit-local-id" value="%d"`, publicFit.ID))
	// Somebody else's public fit still opens unsaved.
	_, body = apptest.GetPage(t, rig, cookie, fmt.Sprintf("/fittings/?public=%d", strangerFit.ID))
	apptest.MustContain(t, "stranger public fit in the editor", body,
		`id="fit-local-id" value="0"`)

	// The public page tells the owner it is theirs, with an edit
	// path, and tells a visitor the copy story instead.
	_, body = apptest.GetPage(t, rig, cookie, fmt.Sprintf("/fittings/public/fit/%d/", publicFit.ID))
	apptest.MustContain(t, "own public page", body,
		"This is your fit", fmt.Sprintf("/fittings/?local=%d#fit-editor", publicFit.ID))
	_, body = apptest.GetPage(t, rig, cookie, fmt.Sprintf("/fittings/public/fit/%d/", strangerFit.ID))
	apptest.MustContain(t, "stranger public page", body, "make your own copy")
}

func postLoadoutsForm(t *testing.T, rig interface{ Handler() http.Handler }, cookie *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	rig.Handler().ServeHTTP(rec, req)
	return rec
}

// TestMyLoadoutsDeleteSavedFit verifies that My Loadouts offers a
// confirmed delete control only for saved EveSynapse fits, keeps the
// user on My Loadouts after deleting their own fit, refuses unsafe
// redirect targets, and leaves other users' fits intact.
func TestMyLoadoutsDeleteSavedFit(t *testing.T) {
	rig := apptest.Build(t, &apptest.CountingTransport{})
	conn := rig.DB()
	q := rig.Queries()
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (3244, 'Medium Shield Extender I', 38)`); err != nil {
		t.Fatalf("seed sde: %v", err)
	}
	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapFittings, esi.Fittings{
		{FittingID: 42, Name: "Alpha Rifter", ShipTypeID: 587,
			Items: []esi.FittingItem{{TypeID: 3244, Quantity: 1, Flag: "MedSlot0"}}},
	})

	publicFit := seedLocalFit(t, q, user.ID, "Harbor Public Rifter", true, false)
	privateFit := seedLocalFit(t, q, user.ID, "Harbor Private Rifter", false, false)
	strangerFit := seedLocalFit(t, q, other.ID, "Stranger Rifter", false, false)

	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Alpha Pilot")

	// Saved EveSynapse fits carry the trashcan delete form and modal dialog.
	code, body := apptest.GetPage(t, rig, cookie, "/fittings/saved/?source=evesynapse")
	if code != http.StatusOK {
		t.Fatalf("GET /fittings/saved/?source=evesynapse: status %d", code)
	}
	apptest.MustContain(t, "/fittings/saved/?source=evesynapse", body,
		`action="/fittings/delete/"`,
		`class="inline-form lo-delete-form"`,
		`class="fit-delete-icon"`,
		`aria-label="Delete Harbor Private Rifter"`,
		`aria-label="Delete Harbor Public Rifter"`,
		`data-fit-delete="Harbor Public Rifter" data-fit-public="1"`,
		fmt.Sprintf(`<input type="hidden" name="id" value="%d">`, privateFit.ID),
		fmt.Sprintf(`<input type="hidden" name="id" value="%d">`, publicFit.ID),
		`<dialog id="lo-delete-dialog" class="modal lo-delete-modal" closedby="any" aria-labelledby="lo-delete-title">`,
		`commandfor="lo-delete-dialog" command="close" autofocus>Cancel</button>`,
	)
	if strings.Contains(body, `data-fit-delete="Harbor Private Rifter" data-fit-public="1"`) {
		t.Errorf("private fit marked as public on delete form")
	}

	// In-game EVE fittings never show the local delete control or modal.
	code, eveBody := apptest.GetPage(t, rig, cookie, "/fittings/saved/?source=eve")
	if code != http.StatusOK {
		t.Fatalf("GET /fittings/saved/?source=eve: status %d", code)
	}
	apptest.MustContain(t, "/fittings/saved/?source=eve", eveBody, "Alpha Rifter")
	if strings.Contains(eveBody, `action="/fittings/delete/"`) || strings.Contains(eveBody, `id="lo-delete-dialog"`) {
		t.Errorf("EVE-only view offered local delete control")
	}

	// Posting another user's fit ID leaves that fit untouched.
	rec := postLoadoutsForm(t, rig, cookie, "/fittings/delete/", url.Values{
		"id":   {strconv.FormatInt(strangerFit.ID, 10)},
		"next": {"/fittings/saved/?source=evesynapse"},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/fittings/saved/?source=evesynapse" {
		t.Fatalf("stranger delete: status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
	strangerRows, err := q.ListLocalFittings(ctx, other.ID)
	if err != nil || len(strangerRows) != 1 || strangerRows[0].ID != strangerFit.ID {
		t.Fatalf("stranger fit missing after another user posted delete: rows=%+v err=%v", strangerRows, err)
	}

	// Unsafe redirect targets fall back to the fitting tool default.
	for _, bad := range []string{"https://evil.example/fittings/", "//evil.example/fittings/", "/market/", "/fittings/saved/\\\nLocation: bad"} {
		rec = postLoadoutsForm(t, rig, cookie, "/fittings/delete/", url.Values{
			"id":   {"0"},
			"next": {bad},
		})
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/fittings/#fit-editor" {
			t.Errorf("unsafe next %q: status %d location %q, want /fittings/#fit-editor", bad, rec.Code, rec.Header().Get("Location"))
		}
	}

	// Deleting the owner's saved fit removes it and returns to My Loadouts.
	wantNext := fmt.Sprintf("/fittings/saved/?character=%d&source=evesynapse", apptest.FixtureCharA)
	rec = postLoadoutsForm(t, rig, cookie, "/fittings/delete/", url.Values{
		"id":   {strconv.FormatInt(privateFit.ID, 10)},
		"next": {wantNext},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != wantNext {
		t.Fatalf("owner delete: status %d location %q, want 303 to %q", rec.Code, rec.Header().Get("Location"), wantNext)
	}
	_, afterBody := apptest.GetPage(t, rig, cookie, "/fittings/saved/?source=evesynapse")
	if strings.Contains(afterBody, "Harbor Private Rifter") {
		t.Errorf("deleted fit still listed on My Loadouts")
	}
	apptest.MustContain(t, "/fittings/saved/ after delete", afterBody,
		"Harbor Public Rifter",
		`<span class="l-stat-label">EveSynapse</span><span class="l-stat-value">1</span>`,
	)
}
