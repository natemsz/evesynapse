package app

import (
	"context"

	"encoding/json"
	"evesynapse/internal/markethistory"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A player's basket is theirs alone: another player gets a 404 for
// it and cannot edit it, and its value history is rebuilt from the
// stored daily rows as quantity times the typical sell.
func TestMarketBasketOwnershipAndValue(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	owner, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, owner.ID, fixtureCharA, "Owner")
	seedCharacter(t, q, other.ID, fixtureCharB, "Other")
	ownerCookie := sessionCookie(t, app, owner.ID, fixtureCharA, "Owner")
	otherCookie := sessionCookie(t, app, other.ID, fixtureCharB, "Other")

	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	for _, s := range []struct {
		id   int64
		name string
		sell float64
	}{{900001, "Basket Test Alpha", 10}, {900002, "Basket Test Beta", 2.5}} {
		if _, err := app.db.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES ($1, $2, 1)`, s.id, s.name); err != nil {
			t.Fatalf("seed type: %v", err)
		}
		if _, err := app.db.ExecContext(ctx, `INSERT INTO market_region_stats_daily (region_id, type_id, day, typical_sell, typical_buy) VALUES (10000002, $1, $2, $3, $3)`, s.id, day, s.sell); err != nil {
			t.Fatalf("seed daily: %v", err)
		}
	}

	code, _ := postForm(t, app, ownerCookie, "/market/baskets/", url.Values{
		"name": {"Mine"}, "region": {"10000002"}, "items": {"basket test alpha 3\nBasket Test Beta x4\nNo Such Thing 1"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("create: status %d, want 303", code)
	}
	var id int64
	if err := app.db.QueryRowContext(ctx, `SELECT id FROM market_baskets WHERE user_id = $1`, owner.ID).Scan(&id); err != nil {
		t.Fatalf("read basket: %v", err)
	}
	var value float64
	var priced, count int64
	if err := app.db.QueryRowContext(ctx, `SELECT value_sell, priced_items, item_count FROM market_basket_values_daily WHERE basket_id = $1 AND day = $2`, id, day).Scan(&value, &priced, &count); err != nil {
		t.Fatalf("read value: %v", err)
	}
	if math.Abs(value-40) > 1e-9 || priced != 2 || count != 2 {
		t.Errorf("value = %v (%d of %d priced), want 40 (3*10 + 4*2.5), 2 of 2", value, priced, count)
	}

	path := "/market/baskets/" + strconv.FormatInt(id, 10) + "/"
	if code, _ := getPage(t, app, ownerCookie, path); code != http.StatusOK {
		t.Errorf("owner GET: status %d, want 200", code)
	}
	if code, _ := getPage(t, app, otherCookie, path); code != http.StatusNotFound {
		t.Errorf("other player GET: status %d, want 404", code)
	}
	if code, _ := postForm(t, app, otherCookie, path+"edit", url.Values{"name": {"Taken"}, "items": {""}}); code != http.StatusNotFound {
		t.Errorf("other player edit: status %d, want 404", code)
	}
}

func TestMarketBasketBuilderAPI(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	owner, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, owner.ID, fixtureCharA, "Owner")
	seedCharacter(t, q, other.ID, fixtureCharB, "Other")
	ownerCookie := sessionCookie(t, app, owner.ID, fixtureCharA, "Owner")
	otherCookie := sessionCookie(t, app, other.ID, fixtureCharB, "Other")
	if _, err := app.db.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES (900001, 'Basket Test Alpha', 1)`); err != nil {
		t.Fatalf("seed type: %v", err)
	}
	if _, err := app.db.ExecContext(ctx, `INSERT INTO market_region_stats (region_id, type_id, typical_sell, typical_buy, updated_at) VALUES (10000002, 900001, 10, 9, now())`); err != nil {
		t.Fatalf("seed stats: %v", err)
	}

	post := func(cookie *http.Cookie, path, body string) (int, []byte) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	get := func(cookie *http.Cookie, path string) (int, []byte) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}

	code, body := post(ownerCookie, "/market/baskets/api", `{"name":"Built","region":10000002,"items":[{"id":900001,"qty":3},{"id":900001,"qty":2}]}`)
	if code != http.StatusCreated {
		t.Fatalf("api create: status %d, body %s", code, body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == 0 {
		t.Fatalf("api create body: %s", body)
	}
	var qty int64
	if err := app.db.QueryRowContext(ctx, `SELECT quantity FROM market_basket_items WHERE basket_id = $1 AND type_id = 900001`, created.ID).Scan(&qty); err != nil || qty != 5 {
		t.Errorf("merged quantity = %d, err %v; want 5", qty, err)
	}

	itemsPath := "/market/baskets/" + strconv.FormatInt(created.ID, 10) + "/items.json"
	code, body = get(ownerCookie, itemsPath)
	if code != http.StatusOK || !strings.Contains(string(body), `"Basket Test Alpha"`) || !strings.Contains(string(body), `"qty":5`) || !strings.Contains(string(body), `"10.00"`) {
		t.Errorf("items.json owner: status %d, body %s", code, body)
	}
	if code, _ := get(otherCookie, itemsPath); code != http.StatusNotFound {
		t.Errorf("items.json other player: status %d, want 404", code)
	}

	code, _ = post(ownerCookie, "/market/baskets/"+strconv.FormatInt(created.ID, 10)+"/api-edit", `{"items":[]}`)
	if code != http.StatusOK {
		t.Fatalf("api edit: status %d", code)
	}
	var n int64
	if err := app.db.QueryRowContext(ctx, `SELECT count(*) FROM market_basket_items WHERE basket_id = $1`, created.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("items after clearing edit = %d, err %v; want 0", n, err)
	}

	code, body = get(ownerCookie, "/market/baskets/prices?region=10000002&types=900001")
	if code != http.StatusOK || !strings.Contains(string(body), `"10.00"`) {
		t.Errorf("prices: status %d, body %s", code, body)
	}
}

// Baskets show up in the watch search next to items and can be
// watched as a whole: the row reads the basket's stored daily
// totals, and only the owner's own baskets (or presets) can be
// watched, never another player's.
func TestWatchlistBasket(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	owner, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, owner.ID, fixtureCharA, "Owner")
	seedCharacter(t, q, other.ID, fixtureCharB, "Other")
	ownerCookie := sessionCookie(t, app, owner.ID, fixtureCharA, "Owner")
	otherCookie := sessionCookie(t, app, other.ID, fixtureCharB, "Other")

	if _, err := app.db.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id) VALUES (900011, 'Watch Pebble', 1)`); err != nil {
		t.Fatalf("seed type: %v", err)
	}
	code, _ := postForm(t, app, ownerCookie, "/market/baskets/", url.Values{
		"name": {"Watch Target"}, "region": {"10000002"}, "items": {"Watch Pebble 1"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("create basket: status %d, want 303", code)
	}
	var basketID int64
	if err := app.db.QueryRowContext(ctx, `SELECT id FROM market_baskets WHERE user_id = $1`, owner.ID).Scan(&basketID); err != nil {
		t.Fatalf("read basket: %v", err)
	}
	// Stored totals: flat a month back, up 20% by yesterday.
	for _, v := range []struct {
		daysAgo int
		value   float64
	}{{-31, 100}, {-8, 100}, {-1, 120}} {
		day := time.Now().UTC().AddDate(0, 0, v.daysAgo).Format("2006-01-02")
		if _, err := app.db.ExecContext(ctx, `INSERT INTO market_basket_values_daily (basket_id, day, value_sell, value_buy, priced_items, item_count) VALUES ($1, $2, $3, $3, 1, 1)`, basketID, day, v.value); err != nil {
			t.Fatalf("seed value: %v", err)
		}
	}

	view := app.buildWatchlistView(ctx, owner.ID, "watch target")
	if len(view.BasketMatches) != 1 || view.BasketMatches[0].ID != basketID || view.BasketMatches[0].Watched {
		t.Fatalf("search matches = %+v, want the basket, unwatched", view.BasketMatches)
	}
	if view.NoMatches {
		t.Fatal("NoMatches set despite a basket match")
	}

	// Another player cannot watch the owner's basket.
	code, _ = postForm(t, app, otherCookie, "/market/watch", url.Values{
		"action": {"add-basket"}, "basket": {strconv.FormatInt(basketID, 10)}, "threshold": {"5"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("foreign add-basket: status %d, want 303", code)
	}
	if rows, _ := q.ListWatchlistBasketsByUser(ctx, other.ID); len(rows) != 0 {
		t.Fatalf("foreign watch landed: %+v", rows)
	}

	code, _ = postForm(t, app, ownerCookie, "/market/watch", url.Values{
		"action": {"add-basket"}, "basket": {strconv.FormatInt(basketID, 10)}, "threshold": {"5"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("add-basket: status %d, want 303", code)
	}
	view = app.buildWatchlistView(ctx, owner.ID, "")
	if len(view.BasketRows) != 1 {
		t.Fatalf("basket rows = %+v, want one", view.BasketRows)
	}
	row := view.BasketRows[0]
	if row.Change7 != markethistory.FormatChangePct(20) || row.Change30 != markethistory.FormatChangePct(20) {
		t.Fatalf("changes = %s / %s, want +20%% both", row.Change7, row.Change30)
	}
	if !row.Moving {
		t.Fatal("a 20% move past a 5% line should read as moving")
	}
	view = app.buildWatchlistView(ctx, owner.ID, "watch target")
	if len(view.BasketMatches) != 1 || !view.BasketMatches[0].Watched {
		t.Fatalf("after watch, match = %+v, want watched", view.BasketMatches)
	}

	code, _ = postForm(t, app, ownerCookie, "/market/watch", url.Values{
		"action": {"remove-basket"}, "basket": {strconv.FormatInt(basketID, 10)},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("remove-basket: status %d, want 303", code)
	}
	if view = app.buildWatchlistView(ctx, owner.ID, ""); len(view.BasketRows) != 0 {
		t.Fatalf("after remove, rows = %+v, want none", view.BasketRows)
	}
}
