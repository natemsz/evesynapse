package app

// ---------------------------------------------------------------------------
// The map's jump planner: the facts the browser plans cyno routes from,
// and the reader's own jump drives and jump bridges.
//
// /map/jump.json is public and the same for everyone: every known-space
// system's true position in light years (the map draws a top-down
// projection; a jump is measured in three dimensions) and every
// published ship with a jump drive, with its base range and fatigue
// multiplier read from the static data export -- CCP's own figures,
// nothing hand-kept. It changes only with the SDE, so it is built once
// and cached by a content hash.
//
// The reader's side rides on /map/mine.json (map_mine.go): each
// character's Jump Drive Calibration and Jump Fuel Conservation levels
// and current jump fatigue, and the Ansiblex jump bridges of their
// corporation (named "From » To - Name", read from a director's
// structures snapshot).
// ---------------------------------------------------------------------------

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	metresPerLY    = 9460730472580800.0
	skillJDC       = 21611 // Jump Drive Calibration: +20% range a level
	skillJFC       = 21610 // Jump Fuel Conservation
	ansiblexTypeID = 35841
)

// Fatigue multipliers where the SDE carries none, by ship group: the
// reductions CCP applies to the distance a jump counts for.
var jumpFatigueByGroup = map[string]float64{
	"Black Ops":               0.25,
	"Jump Freighter":          0.1,
	"Capital Industrial Ship": 0.1,
}

type jumpShip struct {
	TypeID  int64   `json:"t"`
	Name    string  `json:"n"`
	Group   string  `json:"g"`
	Range   float64 `json:"r"` // base light years, before skills
	Fatigue float64 `json:"f"` // distance multiplier for fatigue
}

type jumpPayload struct {
	Ships []jumpShip `json:"ships"`
	// Systems, columnar: ID, region, and position in light years.
	ID     []int64   `json:"id"`
	Region []int64   `json:"region"`
	X      []float64 `json:"x"`
	Y      []float64 `json:"y"`
	Z      []float64 `json:"z"`
}

type jumpCacheEntry struct {
	etag string
	gz   []byte
}

// jumpCache holds each instance's built payload.
var jumpCache = struct {
	sync.Mutex
	m map[*Application]jumpCacheEntry
}{m: map[*Application]jumpCacheEntry{}}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func (app *Application) buildJumpPayload(ctx context.Context) (string, []byte, error) {
	var p jumpPayload
	rows, err := app.queries.ListJumpCoords(ctx)
	if err != nil {
		return "", nil, err
	}
	for _, r := range rows {
		p.ID = append(p.ID, r.SystemID)
		p.Region = append(p.Region, r.RegionID)
		p.X = append(p.X, round3(r.X/metresPerLY))
		p.Y = append(p.Y, round3(r.Y/metresPerLY))
		p.Z = append(p.Z, round3(r.Z/metresPerLY))
	}
	ships, err := app.queries.ListJumpShips(ctx)
	if err != nil {
		return "", nil, err
	}
	for _, s := range ships {
		f := s.FatigueMultiplier
		if f <= 0 {
			f = 1
			if g, ok := jumpFatigueByGroup[s.GroupName]; ok {
				f = g
			}
		}
		p.Ships = append(p.Ships, jumpShip{TypeID: s.TypeID, Name: s.Name, Group: s.GroupName, Range: s.JumpRange, Fatigue: f})
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return hex.EncodeToString(sum[:8]), buf.Bytes(), nil
}

// handleMapJump is GET /map/jump.json. The payload is rebuilt when the
// universe map's own payload changes (an SDE update).
func (app *Application) handleMapJump(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, err := app.queries.GetMapUniverseMeta(ctx)
	if err != nil {
		http.Error(w, "the map has not been built yet", http.StatusServiceUnavailable)
		return
	}
	jumpCache.Lock()
	defer jumpCache.Unlock()
	e := jumpCache.m[app]
	if e.gz == nil || !strings.HasPrefix(e.etag, meta.Etag+"-") {
		sum, gz, err := app.buildJumpPayload(ctx)
		if err != nil {
			logging.Errorf("map: jump payload: %v", err)
			http.Error(w, "jump data unavailable", http.StatusInternalServerError)
			return
		}
		e = jumpCacheEntry{etag: meta.Etag + "-" + sum, gz: gz}
		jumpCache.m[app] = e
	}
	serveMapJSON(w, r, e.etag, e.gz, 3600)
}

// --- The reader's side, on /map/mine.json ---

type mineJumper struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	JDC     int    `json:"jdc"`
	JFC     int    `json:"jfc"`
	Fatigue int64  `json:"fatigue,omitempty"` // when the fatigue wears off
	Known   bool   `json:"known"`             // skills were read
}

type mineBridge struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	Name string `json:"name"`
}

// mineJumpers reads each character's jump skills and fatigue.
func (app *Application) mineJumper(ctx context.Context, id int64, name string) mineJumper {
	j := mineJumper{ID: id, Name: name}
	var skills esi.Skills
	if _, ok := app.snapAt(ctx, id, esi.SnapSkills, &skills); ok {
		j.Known = true
		for _, s := range skills.Skills {
			switch s.SkillID {
			case skillJDC:
				j.JDC = s.ActiveSkillLevel
			case skillJFC:
				j.JFC = s.ActiveSkillLevel
			}
		}
	}
	var fat esi.Fatigue
	if _, ok := app.snapAt(ctx, id, esi.SnapFatigue, &fat); ok {
		j.Fatigue = unixOf(fat.JumpFatigueExpireDate)
	}
	return j
}

// ansiblexBridge reads an Ansiblex's two ends off its name, "From »
// To - Name", ok false when the name is not of that form or names a
// system the SDE does not know.
func (app *Application) ansiblexBridge(ctx context.Context, name string) (mineBridge, bool) {
	i := strings.Index(name, " » ")
	if i <= 0 {
		return mineBridge{}, false
	}
	from, rest := strings.TrimSpace(name[:i]), name[i+len(" » "):]
	to := rest
	if j := strings.Index(rest, " - "); j > 0 {
		to = rest[:j]
	}
	to = strings.TrimSpace(to)
	a, err1 := app.queries.GetSDESystemByName(ctx, from)
	b, err2 := app.queries.GetSDESystemByName(ctx, to)
	if err1 != nil || err2 != nil || a.SystemID == b.SystemID {
		return mineBridge{}, false
	}
	return mineBridge{From: a.SystemID, To: b.SystemID, Name: name}, true
}
