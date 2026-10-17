package app

// Item-page spread tests (spread audit, P0-3): the Spread card
// is priced from the typicals the page already displays, and a
// book too thin for a median to mean anything renders a
// thin-book state instead of a percentage manufactured from
// the extremes.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"evesynapse/internal/esi"
)

// TestMarketItemSpreadThinBookStored is the audit's Offender B
// through the stored-book path: 214 bids (best 90.00, the mass
// at 86.20) against only 3 asks at 1,890 / 2,400 / 3,100. The
// extremes made this a 2000.0% headline KPI; the card now
// names the thin book instead -- with three asks the median
// is still a moon price, so depth, not the basis, is what
// indicts it.
func TestMarketItemSpreadThinBookStored(t *testing.T) {
	transport := &regionBookTransport{books: map[int64][][]esi.MarketOrder{}}
	app, conn, q := buildCorpTestApp(t, transport)
	seedNext1SDE(t, conn)
	for _, region := range marketRegions {
		if region.ID != 10000002 {
			markRegionFresh(t, q, region.ID)
		}
	}
	page := []esi.MarketOrder{}
	orderID := int64(1)
	for i := 0; i < 213; i++ {
		page = append(page, esi.MarketOrder{
			OrderID: orderID, TypeID: 34, IsBuyOrder: true,
			Price: 86.20, VolumeRemain: 100,
			LocationID: 60003760, SystemID: 30000142,
		})
		orderID++
	}
	page = append(page, esi.MarketOrder{
		OrderID: orderID, TypeID: 34, IsBuyOrder: true,
		Price: 90.00, VolumeRemain: 100,
		LocationID: 60003760, SystemID: 30000142,
	})
	orderID++
	for _, price := range []float64{1890, 2400, 3100} {
		page = append(page, esi.MarketOrder{
			OrderID: orderID, TypeID: 34, IsBuyOrder: false,
			Price: price, VolumeRemain: 100,
			LocationID: 60003760, SystemID: 30000142,
		})
		orderID++
	}
	transport.books[10000002] = [][]esi.MarketOrder{page}
	sweepUntilIdle(t, app)

	code, body := getSwapPage(t, app, nil, "/market/?type=34")
	if code != http.StatusOK {
		t.Fatalf("GET /market/?type=34 (thin book): status %d, want 200", code)
	}
	mustContain(t, "/market/?type=34 (thin book)", body,
		"Thin book",
		"only 3 sell orders",
	)
	if strings.Contains(body, "2000.0%") {
		t.Error("thin book still renders the manufactured 2000.0% spread")
	}
	if strings.Contains(body, `spread-v">`) {
		t.Error("thin book still renders a spread percentage")
	}
}

// spreadBookTransport serves one fixed live book for type 34
// in The Forge, plus the guide prices the signed-in item view
// reads.
type spreadBookTransport struct {
	calls atomic.Int64
	book  string
}

func (s *spreadBookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	path := req.URL.Path
	respond := func(code int, body string) *http.Response {
		return &http.Response{
			StatusCode: code,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}
	switch {
	case strings.Contains(path, "/orders/"):
		return respond(200, s.book), nil
	case path == "/markets/prices/":
		return respond(200, `[{"type_id":34,"average_price":95.0,"adjusted_price":90.0}]`), nil
	default:
		return respond(404, `{"error":"unexpected path `+path+`"}`), nil
	}
}

// TestMarketItemSpreadPricedFromTypicals: a deep book whose
// extremes lie about the gap. One bait ask at 87.50 sits a
// hair over the best bid at 87.40, so the old best-based card
// read 0.1%; the mass of the book (19 asks at 100.00, 19 bids
// at 87.00) puts the typicals at 100.00 / 87.00, and the card
// now quotes their 14.9%.
func TestMarketItemSpreadPricedFromTypicals(t *testing.T) {
	orders := []esi.MarketOrder{}
	orderID := int64(1)
	add := func(isBuy bool, price float64, n int) {
		for i := 0; i < n; i++ {
			orders = append(orders, esi.MarketOrder{
				OrderID: orderID, TypeID: 34, LocationID: 60003760, SystemID: 30000142,
				IsBuyOrder: isBuy, Price: price, VolumeTotal: 100, VolumeRemain: 100,
				Range: "region", Issued: "2026-10-01T00:00:00Z", Duration: 90,
			})
			orderID++
		}
	}
	add(false, 87.50, 1)
	add(false, 100.00, 19)
	add(true, 87.40, 1)
	add(true, 87.00, 19)
	raw, err := json.Marshal(orders)
	if err != nil {
		t.Fatalf("marshal book: %v", err)
	}
	transport := &spreadBookTransport{book: string(raw)}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	if _, err := conn.Exec(`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (34, 'Tritanium', 1, 1, 1)`); err != nil {
		t.Fatalf("seed SDE type: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/market/?type=34")
	if code != http.StatusOK {
		t.Fatalf("item page: status %d", code)
	}
	mustContain(t, "/market/?type=34 (typical spread)", body,
		`spread-v">14.9%`,
		"Between typical buy and typical sell",
	)
	if strings.Contains(body, `spread-v">0.1%`) {
		t.Error("spread still priced from the best extremes (0.1%), want the typicals' 14.9%")
	}
}
