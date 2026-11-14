package app

import (
	"encoding/json"
	"net/http"
	"time"
)

// handleMapKills is GET /map/kills.json: the newest public kills, for
// the map's live kills layer to place before /intel/kills/stream adds
// each new one. It reads the in-memory ring of recent kills
// (killboard_recent.go), so a warm ring costs no database read.
// Low-memory mode keeps no ring and answers an empty list.
func (app *Application) handleMapKills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	kills, _ := app.getRecentKills(ctx, time.Now().Add(time.Hour).UTC(), 1<<62, 0, false, recentKillsCapacity)
	rows := make([]killRow, 0, len(kills))
	if len(kills) > 0 {
		names := app.newKillNames(ctx)
		for _, k := range kills {
			rows = append(rows, names.row(k))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Kills []killRow `json:"kills"`
		Live  bool      `json:"live"`
	}{rows, !app.cfg.lowRAMMode})
}
