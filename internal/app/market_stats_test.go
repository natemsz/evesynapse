package app

// Honest prices: the item page quotes the median and
// 9-in-10 band of the whole regional book alongside the bests,
// so one joke order cannot stand in for the market. Unit tests
// pin the statistics; the page test drives a book with a scam
// order at each extreme and asserts the quoted prices ignore
// them while the bests still report them honestly.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func bookOrders(prices ...float64) []esi.MarketOrder {
	orders := make([]esi.MarketOrder, len(prices))
	for i, p := range prices {
		orders[i] = esi.MarketOrder{
			OrderID: int64(i + 1), TypeID: 34, LocationID: 60003760,
			SystemID: 30000142, Price: p, VolumeRemain: 100,
		}
	}
	return orders
}

func TestSummarizeBook(t *testing.T) {
	if _, ok := summarizeBook(nil); ok {
		t.Fatal("empty book: ok = true, want false")
	}

	one, ok := summarizeBook(bookOrders(42))
	if !ok || one.Median != 42 || one.Low90 != 42 || one.High90 != 42 {
		t.Fatalf("single order: %+v ok=%v, want all 42", one, ok)
	}

	// Ten orders, the top one a 400 ISK joke among 4–8 ISK asks:
	// the median and the 9-in-10 band must not move for it.
	sells, ok := summarizeBook(bookOrders(4, 4.5, 5, 5.5, 6, 6.5, 7, 7.5, 8, 400))
	if !ok {
		t.Fatal("sell book: ok = false")
	}
	if sells.Median != 6.25 {
		t.Errorf("sell median = %v, want 6.25", sells.Median)
	}
	if sells.High90 != 8 {
		t.Errorf("sell 9-in-10 band = %v, want 8 (joke order excluded)", sells.High90)
	}

	// Five bids with a 500 ISK bait on top: best buy is poisoned
	// by construction; the typical bid must not be.
	buys, ok := summarizeBook(bookOrders(3, 3.5, 4, 4.5, 500))
	if !ok {
		t.Fatal("buy book: ok = false")
	}
	if buys.Median != 4 {
		t.Errorf("buy median = %v, want 4", buys.Median)
	}
	if buys.Low90 != 3 {
		t.Errorf("buy 9-in-10 band = %v, want 3", buys.Low90)
	}
}

// honestBookTransport serves the price guide and one regional
// order book; everything else fails loudly.
type honestBookTransport struct{ calls atomic.Int64 }

func (s *honestBookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	respond := func(v any) *http.Response {
		b, _ := json.Marshal(v)
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(string(b))),
		}
	}
	switch req.URL.Path {
	case "/markets/prices/":
		return respond([]esi.MarketPrice{
			{TypeID: 34, AveragePrice: 5.9, AdjustedPrice: 5},
		}), nil
	case "/markets/10000002/orders/":
		book := bookOrders(4, 4.5, 5, 5.5, 6, 6.5, 7, 7.5, 8, 400)
		for i, p := range []float64{3, 3.5, 4, 4.5, 500} {
			o := esi.MarketOrder{
				OrderID: int64(100 + i), TypeID: 34, LocationID: 60003760,
				SystemID: 30000142, Price: p, VolumeRemain: 50, IsBuyOrder: true,
			}
			book = append(book, o)
		}
		return respond(book), nil
	}
	return &http.Response{
		StatusCode: http.StatusInternalServerError, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"error":"fixture"}`)),
	}, nil
}

func TestMarketPageHonestPrices(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &honestBookTransport{})
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/market/?type=34&region=10000002")
	if code != http.StatusOK {
		t.Fatalf("market page: status %d", code)
	}
	mustContain(t, "/market/ honest prices", body,
		"Best sell: <strong>4.00 ISK</strong>",
		"Best buy: <strong>500.00 ISK</strong>",
		"Typical sell: <strong>6.25 ISK</strong>",
		"9 in 10 sell orders at or under 8.00 ISK",
		"Typical buy: <strong>4.00 ISK</strong>",
		"9 in 10 buy orders at or over 3.00 ISK",
	)
}
