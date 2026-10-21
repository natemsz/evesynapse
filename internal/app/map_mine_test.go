package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestMapMine: the reader's own data comes back by system -- where
// their pilots are, clones at stations and structures, orders, assets
// valued at the top of their container chain -- only for them, and
// nothing for a visitor.
func TestMapMine(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita IV - Moon 4 - Caldari Navy Assembly Plant', 30000142) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO structure_context (structure_id, owner_corporation_id, system_id, type_id, updated_at) VALUES (1030000000001, 1, 30002187, 35832, now()) ON CONFLICT DO NOTHING`)
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, 90000301, "Data Pilot")
	cookie := sessionCookie(t, app, u.ID, 90000301, "Data Pilot")

	seedSnapshot(t, q, 90000301, esi.SnapLocation, esi.Location{SolarSystemID: 30000142, StationID: 60003760})
	seedSnapshot(t, q, 90000301, esi.SnapOnline, esi.Online{Online: true})
	seedSnapshot(t, q, 90000301, esi.SnapClones, esi.Clones{
		HomeLocation: esi.CloneHome{LocationID: 60003760, LocationType: "station"},
		JumpClones:   []esi.JumpClone{{JumpCloneID: 1, LocationID: 1030000000001, LocationType: "structure", Implants: []int64{1, 2}}},
	})
	seedSnapshot(t, q, 90000301, esi.SnapOrders, []esi.MarketOrder{{OrderID: 1, TypeID: 34, LocationID: 60003760, SystemID: 30000142, Price: 5, VolumeRemain: 1000}})
	// A ship in a station holding cargo: both count in the station's system.
	seedSnapshot(t, q, 90000301, esi.SnapAssets, []esi.Asset{
		{ItemID: 10, TypeID: 587, Quantity: 1, LocationID: 60003760, LocationType: "station", LocationFlag: "Hangar", IsSingleton: true},
		{ItemID: 11, TypeID: 34, Quantity: 100, LocationID: 10, LocationType: "item", LocationFlag: "Cargo"},
	})
	// Valued in the house order: The Forge's swept typical sells.
	for typeID, sell := range map[int64]float64{587: 1000000, 34: 5} {
		if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{RegionID: defaultMarketRegion, TypeID: typeID, BestSell: sell, TypicalSell: sell, SellOrders: 5, SellVolume: 1000, UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	code, body := getPage(t, app, cookie, "/map/mine.json")
	if code != http.StatusOK {
		t.Fatalf("mine.json: %d %s", code, body)
	}
	var d minePayload
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Pilots) != 1 || d.Pilots[0].Sys != 30000142 || !d.Pilots[0].Online || !d.Pilots[0].Docked {
		t.Errorf("pilots: %+v", d.Pilots)
	}
	if len(d.Clones) != 2 {
		t.Errorf("a home at a station and a jump clone at a structure: %+v", d.Clones)
	}
	for _, c := range d.Clones {
		if c.Home && c.Sys != 30000142 || !c.Home && (c.Sys != 30002187 || c.Implants != 2) {
			t.Errorf("clone placed wrong: %+v", c)
		}
	}
	if len(d.Market) != 1 || d.Market[0].Sell != 1 || d.Market[0].ISKSell != 5000 {
		t.Errorf("market: %+v", d.Market)
	}
	if len(d.Assets) != 1 || d.Assets[0].Sys != 30000142 || d.Assets[0].V != 1000000+100*5 {
		t.Errorf("the ship and its cargo roll up to the station's system at typical sells: %+v", d.Assets)
	}

	if code, _ := getPage(t, app, &http.Cookie{Name: "nobody", Value: "x"}, "/map/mine.json"); code != http.StatusUnauthorized {
		t.Errorf("a visitor gets nothing: %d", code)
	}
}
