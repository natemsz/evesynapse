package app

// Parity for this package's swapped decode sites: the app-side
// killmail type and a snapshot kind read back through the swapped
// snapshot helper must decode identically under goccy/go-json
// (what the sites now use) and encoding/json.

import (
	"context"
	stdjson "encoding/json"
	"reflect"
	"testing"

	"evesynapse/internal/esi"

	goccyjson "github.com/goccy/go-json"
)

func TestAppDecoderParityKillmail(t *testing.T) {
	raw := killJSON(700001)

	var std killmailFull
	if err := stdjson.Unmarshal([]byte(raw), &std); err != nil {
		t.Fatalf("encoding/json decode: %v", err)
	}
	var goc killmailFull
	if err := goccyjson.Unmarshal([]byte(raw), &goc); err != nil {
		t.Fatalf("goccy/go-json decode: %v", err)
	}
	if !reflect.DeepEqual(std, goc) {
		t.Fatalf("killmail decodes differ:\nencoding/json: %+v\ngoccy/go-json:  %+v", std, goc)
	}
}

func TestAppDecoderParitySnapshotOrders(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Parity Tester")

	payload := `[{"order_id":7000000001,"type_id":34,"location_id":60003760,"region_id":10000002,` +
		`"is_buy_order":true,"is_corporation":false,"price":123.45,"volume_total":1000,"volume_remain":750,` +
		`"min_volume":1,"range":"region","issued":"2026-11-01T12:34:56Z","duration":90,"escrow":92587.5},` +
		`{"order_id":7000000002,"type_id":35,"location_id":60003760,"region_id":10000002,` +
		`"is_buy_order":false,"is_corporation":true,"price":0.01,"volume_total":5,"volume_remain":5,` +
		`"min_volume":1,"range":"station","issued":"2026-11-01T12:34:56Z","duration":30,"escrow":0}]`
	seedSnapshot(t, q, fixtureCharA, esi.SnapOrders, payload)

	got, ok := app.loadOrdersSnapshot(ctx, fixtureCharA)
	if !ok {
		t.Fatal("orders snapshot did not decode through the helper")
	}
	var want []esi.CharOrder
	if err := stdjson.Unmarshal([]byte(payload), &want); err != nil {
		t.Fatalf("encoding/json decode: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("snapshot decodes differ:\nencoding/json: %+v\nthrough helper: %+v", want, got)
	}
}
