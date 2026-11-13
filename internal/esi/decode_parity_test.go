package esi

// Parity fixtures: wire-shaped ESI payloads (the field sets ESI
// sends, including fields the DTOs ignore, because a decoder
// still has to scan them) and the request shapes the client
// encodes.

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	goccyjson "github.com/goccy/go-json"
)

func parityOrdersPage(n int) []byte {
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"duration":90,"is_buy_order":%t,"issued":"2026-11-01T12:34:56Z","location_id":60003760,"min_volume":1,"order_id":%d,"price":%.2f,"range":"region","system_id":30000142,"type_id":%d,"volume_remain":%d,"volume_total":%d}`,
			i%2 == 0, int64(6_900_000_000+i), 123.45+float64(i%97), int64(34+i%500),
			int64(500+i%1000), int64(1000+i%1000))
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func parityAssetsPage(n int) []byte {
	flags := []string{"Hangar", "CorpSAG1", "Deliveries", "Impound", "HangarQuery"}
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		singleton := i%5 == 0
		qty := int64(250 + i%5000)
		if singleton {
			qty = 1
		}
		fmt.Fprintf(&b, `{"is_blueprint_copy":false,"is_singleton":%t,"item_id":%d,"location_flag":%q,"location_id":60003760,"location_type":"station","quantity":%d,"type_id":%d}`,
			singleton, int64(1_040_000_000_000+i), flags[i%len(flags)], qty, int64(34+i%5000))
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func parityKillmail() []byte {
	var b strings.Builder
	b.WriteString(`{"attackers":[`)
	for i := range 20 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"alliance_id":99000002,"character_id":%d,"corporation_id":98000002,"damage_done":%d,"faction_id":0,"final_blow":%t,"security_status":%.1f,"ship_type_id":11371,"weapon_type_id":2873}`,
			int64(9002+i), int64(4500-i*200), i == 0, -2.5+float64(i)*0.3)
	}
	b.WriteString(`],"killmail_id":139700392,"killmail_time":"2026-11-05T19:42:11Z","solar_system_id":30000142,"victim":{"alliance_id":99000001,"character_id":9001,"corporation_id":98000001,"damage_taken":98765,"items":[`)
	for i := range 40 {
		if i > 0 {
			b.WriteByte(',')
		}
		if i == 10 {
			// A container with nested contents, as on real mails.
			b.WriteString(`{"flag":5,"item_type_id":3465,"quantity_dropped":1,"singleton":0,"items":[{"flag":0,"item_type_id":999,"quantity_dropped":1,"singleton":2},{"flag":0,"item_type_id":178,"quantity_destroyed":100,"singleton":0},{"flag":0,"item_type_id":27339,"quantity_dropped":4,"singleton":0}]}`)
			continue
		}
		fmt.Fprintf(&b, `{"flag":%d,"item_type_id":%d,"quantity_destroyed":%d,"singleton":0}`, 27+i, int64(2873+i*7), int64(1+i%3))
	}
	b.WriteString(`],"ship_type_id":587}}`)
	return []byte(b.String())
}

func parityCharacter() []byte {
	return []byte(`{"alliance_id":99000001,"birthday":"2009-12-24T11:03:00Z","bloodline_id":8,"corporation_id":98000001,"description":"Solo empire builder. <font size=\"12\">Occasionally explodes.</font>","faction_id":0,"gender":"male","name":"Burzrujat","race_id":4,"security_status":-1.27,"title":"CEO"}`)
}

// The skillqueue entries exercise the pointer field: a value, an
// explicit null, and the field absent altogether.
const paritySkillqueue = `[
{"skill_id":3300,"finished_level":5,"queue_position":0,"start_date":"2026-11-01T12:00:00Z","finish_date":"2026-11-03T12:00:00Z","training_start_sp":8000,"level_start_sp":0,"level_end_sp":45255},
{"skill_id":3301,"finished_level":4,"queue_position":1,"start_date":"2026-11-03T12:00:00Z","finish_date":"2026-11-04T12:00:00Z","training_start_sp":null,"level_start_sp":45255,"level_end_sp":90510},
{"skill_id":3302,"finished_level":3,"queue_position":2,"level_start_sp":90510,"level_end_sp":181020}
]`

// The clones payload exercises nested slices: implant lists
// inside jump clones inside the response object.
const parityClones = `{"home_location":{"location_id":60003760,"location_type":"station"},"jump_clones":[{"implants":[12003,12004,12005],"jump_clone_id":12345678,"location_id":60003760,"location_type":"station","name":""},{"implants":[],"jump_clone_id":12345679,"location_id":1028858195912,"location_type":"structure","name":"Travel clone"}],"last_clone_jump_date":"2026-10-30T11:00:00Z"}`

func decodeBoth[T any](t *testing.T, data []byte) (std, goc T) {
	t.Helper()
	if err := stdjson.Unmarshal(data, &std); err != nil {
		t.Fatalf("encoding/json decode: %v", err)
	}
	if err := goccyjson.Unmarshal(data, &goc); err != nil {
		t.Fatalf("goccy/go-json decode: %v", err)
	}
	return std, goc
}

func marshalBoth(t *testing.T, v any) {
	t.Helper()
	std, err := stdjson.Marshal(v)
	if err != nil {
		t.Fatalf("encoding/json encode: %v", err)
	}
	goc, err := goccyjson.Marshal(v)
	if err != nil {
		t.Fatalf("goccy/go-json encode: %v", err)
	}
	if !bytes.Equal(std, goc) {
		t.Errorf("encoded bytes differ:\nencoding/json %s\ngoccy/go-json  %s", std, goc)
	}
}

// paritySmallPayload is a small request struct whose description
// carries the characters JSON escapes in strings.
type paritySmallPayload struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ShipTypeID  int64  `json:"ship_type_id"`
}

// TestDecoderParity pins the contract that lets this package use
// goccy/go-json under the encoding/json name: for every payload
// shape the client decodes or encodes, the two libraries agree,
// deeply equal values on decode and byte-equal output on encode.
func TestDecoderParity(t *testing.T) {
	t.Run("decode market orders page", func(t *testing.T) {
		std, goc := decodeBoth[[]MarketOrder](t, parityOrdersPage(1000))
		if !reflect.DeepEqual(std, goc) {
			t.Errorf("orders differ: %d vs %d entries", len(std), len(goc))
			for i := range std {
				if std[i] != goc[i] {
					t.Errorf("first differing order %d: encoding/json %+v vs goccy %+v", i, std[i], goc[i])
					break
				}
			}
		}
	})

	t.Run("decode assets page", func(t *testing.T) {
		std, goc := decodeBoth[[]Asset](t, parityAssetsPage(1000))
		if !reflect.DeepEqual(std, goc) {
			t.Errorf("assets differ: %d vs %d entries", len(std), len(goc))
			for i := range std {
				if std[i] != goc[i] {
					t.Errorf("first differing asset %d: encoding/json %+v vs goccy %+v", i, std[i], goc[i])
					break
				}
			}
		}
	})

	t.Run("decode killmail", func(t *testing.T) {
		std, goc := decodeBoth[Killmail](t, parityKillmail())
		if !reflect.DeepEqual(std, goc) {
			t.Errorf("killmail differs:\nencoding/json %+v\ngoccy %+v", std, goc)
		}
	})

	t.Run("decode character", func(t *testing.T) {
		std, goc := decodeBoth[Character](t, parityCharacter())
		if !reflect.DeepEqual(std, goc) {
			t.Errorf("character differs: encoding/json %+v vs goccy %+v", std, goc)
		}
	})

	t.Run("decode skillqueue pointer fields", func(t *testing.T) {
		std, goc := decodeBoth[Skillqueue](t, []byte(paritySkillqueue))
		if !reflect.DeepEqual(std, goc) {
			t.Fatalf("skillqueue differs: encoding/json %+v vs goccy %+v", std, goc)
		}
		if goc[0].TrainingStartSP == nil || *goc[0].TrainingStartSP != 8000 {
			t.Errorf("entry 0 training_start_sp: got %v, want a pointer to 8000", goc[0].TrainingStartSP)
		}
		if goc[1].TrainingStartSP != nil {
			t.Errorf("entry 1 training_start_sp (null): got %v, want nil", *goc[1].TrainingStartSP)
		}
		if goc[2].TrainingStartSP != nil {
			t.Errorf("entry 2 training_start_sp (absent): got %v, want nil", *goc[2].TrainingStartSP)
		}
	})

	t.Run("decode clones nested slices", func(t *testing.T) {
		std, goc := decodeBoth[Clones](t, []byte(parityClones))
		if !reflect.DeepEqual(std, goc) {
			t.Errorf("clones differ:\nencoding/json %+v\ngoccy %+v", std, goc)
		}
	})

	t.Run("encode ID batch", func(t *testing.T) {
		ids := make([]int64, 1000)
		for i := range ids {
			ids[i] = 90000000 + int64(i)
		}
		marshalBoth(t, ids)
	})

	t.Run("encode name batch", func(t *testing.T) {
		names := make([]string, 100)
		for i := range names {
			names[i] = fmt.Sprintf("Pilot Name %d", i)
		}
		marshalBoth(t, names)
	})

	t.Run("encode merged raw pages", func(t *testing.T) {
		entry := func(i int) string {
			return fmt.Sprintf(`{"id":%d,"amount":%.2f,"balance":%.2f,"date":"2026-11-01T12:34:56Z","description":"Market escrow","ref_type":"market_escrow","first_party_id":%d,"second_party_id":%d}`,
				7000000000+i, -1234.56, 987654.32, 90000000+i, 10000000+i)
		}
		stdPages := make([]stdjson.RawMessage, 1000)
		gocPages := make([]goccyjson.RawMessage, 1000)
		for i := range stdPages {
			stdPages[i] = stdjson.RawMessage(entry(i))
			gocPages[i] = goccyjson.RawMessage(entry(i))
		}
		std, err := stdjson.Marshal(stdPages)
		if err != nil {
			t.Fatalf("encoding/json encode: %v", err)
		}
		goc, err := goccyjson.Marshal(gocPages)
		if err != nil {
			t.Fatalf("goccy/go-json encode: %v", err)
		}
		if !bytes.Equal(std, goc) {
			t.Errorf("merged pages differ: encoding/json %d bytes, goccy %d bytes", len(std), len(goc))
		}
	})

	t.Run("encode small struct with escapable characters", func(t *testing.T) {
		marshalBoth(t, paritySmallPayload{
			Name:        strings.Repeat("A", 40),
			Description: `Test <fit> & "quoted"`,
			ShipTypeID:  587,
		})
	})
}
