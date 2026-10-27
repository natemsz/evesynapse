package app

// ---------------------------------------------------------------------------
// Axon's preview and overrides: what the site would make of a paste
// before it is sent, and the player's word on it when they disagree.
//
//	POST /api/intel/v1/preview   {"text": "...", "speaker": "..."}
//
// answers with the paste's kind (a scan kind, or "chat") and the
// system the site would file it under, with where that system came
// from: the scan's celestials, a line naming it, or the speaker's or
// poster's stored location. It stores nothing: the readings it runs
// are the posting path's own (scan.Parse, readDScan, intelchat), on
// stored rows only, with nothing fetched. Axon's overlay shows the
// answer beside the Send button, so the player sees the kind and the
// system before the paste goes.
//
// A send may then carry overrides on the lines API ("kind",
// "system"). The site still reads the paste itself, under them
// (postChatLinesWith); a send carrying one is also logged beside
// what was detected (schema 094), a labelled example for the
// classifier the residue corpus is building toward.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelchat"
	"evesynapse/internal/logging"
	"evesynapse/internal/scan"
)

// submitKindChat is the kind override that reads a paste as chat
// lines only: a roster the player knows is chatter, never a scan.
const submitKindChat = "chat"

// submitOverride is a submission's overrides: the kind the paste
// is (a scan kind or submitKindChat), and the system it is about.
// The zero value overrides nothing.
type submitOverride struct {
	Kind     string
	SystemID int64
}

// scanKind is the override's scan kind, or "" when it names none.
func (ov submitOverride) scanKind() scan.Kind {
	switch k := scan.Kind(ov.Kind); k {
	case scan.KindDScan, scan.KindLocal, scan.KindFleet, scan.KindProbe:
		return k
	}
	return ""
}

func (ov submitOverride) any() bool { return ov.Kind != "" || ov.SystemID > 0 }

// validSubmitKind is whether a kind override is one the site reads.
func validSubmitKind(k string) bool {
	return k == "" || k == submitKindChat || submitOverride{Kind: k}.scanKind() != ""
}

// intelPreview is what the site would make of a paste.
type intelPreview struct {
	Kind       string `json:"kind"`
	SystemID   int64  `json:"system_id,omitempty"`
	SystemName string `json:"system_name,omitempty"`
	// SystemFrom is where the system came from: "scan" (its
	// celestials), "line" (a chat line names it), "speaker" or
	// "poster" (a stored location), or "" with no system.
	SystemFrom string `json:"system_from,omitempty"`
	// Systems is how many systems a chat paste's lines name; the
	// preview shows the first.
	Systems int `json:"systems,omitempty"`
}

// previewSubmission reads raw the way the posting path would and
// says what it found, storing nothing.
func (app *Application) previewSubmission(ctx context.Context, raw, speaker string, posterID int64) intelPreview {
	var pv intelPreview
	if p, err := scan.Parse(raw); err == nil {
		pv.Kind = string(p.Kind)
		if p.Kind == scan.KindDScan {
			if _, detected, _ := app.readDScan(ctx, p.DScan); detected != 0 {
				pv.SystemID, pv.SystemFrom = detected, "scan"
			}
		}
	} else {
		pv.Kind = submitKindChat
		o := intelchat.Parse(raw, app.systemLookup(ctx), time.Now())
		seen := map[int64]bool{}
		for _, r := range o.Reports {
			if seen[r.SystemID] {
				continue
			}
			seen[r.SystemID] = true
			if pv.SystemID == 0 {
				pv.SystemID, pv.SystemFrom = r.SystemID, "line"
			}
			if speaker == "" {
				speaker = r.Speaker
			}
		}
		pv.Systems = len(seen)
	}
	if pv.SystemID == 0 {
		if id := app.speakerSystemID(ctx, speaker); id != 0 {
			pv.SystemID, pv.SystemFrom = id, "speaker"
		} else if id := app.posterSystemID(ctx, posterID); id != 0 {
			pv.SystemID, pv.SystemFrom = id, "poster"
		}
	}
	if pv.SystemID != 0 {
		if sys, err := app.queries.GetSDESystem(ctx, pv.SystemID); err == nil {
			pv.SystemName = sys.Name
		}
	}
	return pv
}

// handleAPIPreview is POST /api/intel/v1/preview.
func (app *Application) handleAPIPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentityFor(ctx, tokenUser(ctx), 0)
	var body struct {
		Text    string `json:"text"`
		Speaker string `json:"speaker"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, intelMaxPaste)).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		intelAPIError(w, http.StatusBadRequest, "send {\"text\": \"...\"}, up to 64 KB")
		return
	}
	speaker := app.accountSpeaker(ctx, id.userID, body.Speaker)
	pv := app.previewSubmission(ctx, body.Text, speaker, id.posterID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		OK bool `json:"ok"`
		intelPreview
	}{true, pv})
}

// logSubmitLabel records a submission sent with an override beside
// what the site detected for the same text (pv, read before it was
// posted). A failure is logged and costs only the label: the
// submission itself already went.
func (app *Application) logSubmitLabel(ctx context.Context, channelID, userID int64, raw string, pv intelPreview, ov submitOverride, posted int) {
	if err := app.queries.InsertIntelSubmitLabel(ctx, db.InsertIntelSubmitLabelParams{
		ChannelID: channelID, UserID: userID, Raw: raw,
		DetectedKind: pv.Kind, DetectedSystemID: pv.SystemID,
		OverrideKind: ov.Kind, OverrideSystemID: ov.SystemID,
		Posted: int32(posted), CreatedAt: time.Now().UTC(),
	}); err != nil {
		logging.Warnf("axon: log submission label: %v", err)
	}
}
