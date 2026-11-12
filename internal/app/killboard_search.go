package app

// ---------------------------------------------------------------------------
// Killboard name search: the box at the head of the feed. One merged
// lookup over the local name sources (warmed pilot, corporation and
// alliance records; SDE systems, regions and ship types) answers the
// suggestion feed and the search form alike: every hit names one of
// the feed's scopes, and a pick (or the form's redirect) lands on
// /intel/kills/?<kind>=<id>. Nothing here fetches: a pilot name no
// record knows yet goes through the same name-resolution want the
// banner search notes (notePilotNameSearch), so a real but unwarmed
// pilot still resolves, a moment later.
// ---------------------------------------------------------------------------

import (
	"context"
	json "github.com/goccy/go-json"
	"net/http"
	"sort"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// kbSearchItem is one killboard suggestion: the scope a pick opens,
// the name, and its context line (a corporation's or alliance's
// ticker, a system's region, a ship's class). A system's security
// rides in Sec and SecTier, as the banner search's hits carry it. A
// "pilot-pending" item is the not-yet-resolved name search: it
// names no scope and is never a pick.
type kbSearchItem struct {
	Kind    string `json:"kind"` // "character" | "corporation" | "alliance" | "system" | "region" | "ship" | "pilot-pending"
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Sub     string `json:"sub,omitempty"`
	Sec     string `json:"sec,omitempty"`
	SecTier int    `json:"secTier,omitempty"`
}

// kbSearchLimit caps the merged suggestions: the dropdown stays
// thumb-sized however many kinds match.
const kbSearchLimit = 10

type kbSearchCand struct {
	item kbSearchItem
	tier int
}

// killboardSearch merges one query's hits from the six lookups,
// each bounded, under the app's name-match discipline (prefix
// before substring, then name), capped at kbSearchLimit. Kinds
// gather in the feed's own scope order, so a dead tie between two
// kinds breaks the way the handler itself would scope it. It also
// reports the pilot-name resolution state: pending while an
// unwarmed pilot's name is being resolved (the want is noted here,
// exactly as the banner search notes it), with a just-resolved
// pilot folded into the hits.
func (app *Application) killboardSearch(ctx context.Context, q string, userID int64) (hits []kbSearchItem, pending bool) {
	q = strings.TrimSpace(q)
	if len(q) < 2 {
		return nil, false
	}
	var cands []kbSearchCand
	// addFor ranks one kind's hits against the query they answer.
	addFor := func(mq string, items []kbSearchItem) {
		sort.SliceStable(items, func(i, j int) bool {
			ti, _ := nameMatchTier(items[i].Name, mq)
			tj, _ := nameMatchTier(items[j].Name, mq)
			if ti != tj {
				return ti < tj
			}
			return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
		})
		if len(items) > kbSearchLimit {
			items = items[:kbSearchLimit]
		}
		for _, it := range items {
			tier, _ := nameMatchTier(it.Name, mq)
			cands = append(cands, kbSearchCand{item: it, tier: tier})
		}
	}
	add := func(items []kbSearchItem) { addFor(q, items) }

	// The static data kinds: systems, regions, ships. When none
	// match as typed they are read again with the misspellings
	// fixed (search_typos.go); people are never corrected.
	var shipHits []searchHit
	sdeKinds := func(mq string) int {
		found := 0
		if rows, err := app.queries.SuggestSDESystems(ctx, db.SuggestSDESystemsParams{Q: mq, Lim: kbSearchLimit}); err == nil {
			var items []kbSearchItem
			for _, row := range rows {
				if _, ok := nameMatchTier(row.Name, mq); !ok {
					continue
				}
				sec, tier := secRound(row.Security)
				items = append(items, kbSearchItem{Kind: "system", ID: row.SystemID, Name: row.Name, Sub: row.RegionName, Sec: sec, SecTier: tier})
			}
			found += len(items)
			addFor(mq, items)
		} else {
			logging.Errorf("killboard search: systems for %q: %v", mq, err)
		}
		if rows, err := app.queries.SuggestSDERegions(ctx, db.SuggestSDERegionsParams{Q: mq, Lim: kbSearchLimit}); err == nil {
			var items []kbSearchItem
			for _, row := range rows {
				items = append(items, kbSearchItem{Kind: "region", ID: row.RegionID, Name: row.Name})
			}
			found += len(items)
			addFor(mq, items)
		} else {
			logging.Errorf("killboard search: regions for %q: %v", mq, err)
		}
		if rows, err := app.queries.SuggestSDEShips(ctx, db.SuggestSDEShipsParams{Q: mq, Lim: kbSearchLimit}); err == nil {
			var items []kbSearchItem
			for _, row := range rows {
				items = append(items, kbSearchItem{Kind: "ship", ID: row.TypeID, Name: row.Name, Sub: row.GroupName})
				if mq == q {
					// Only a ship named as typed tells the pilot
					// lookup the text is not a pilot's name.
					shipHits = append(shipHits, searchHit{Kind: "item", ID: row.TypeID, Name: row.Name})
				}
			}
			found += len(items)
			addFor(mq, items)
		} else {
			logging.Errorf("killboard search: ships for %q: %v", mq, err)
		}
		return found
	}
	if sdeKinds(q) == 0 {
		if fix, ok := app.correctSearch(q); ok {
			sdeKinds(fix)
		}
	}

	charHit := false
	if rows, err := app.queries.SearchPilotRecordsByName(ctx, q); err == nil {
		var items []kbSearchItem
		for _, row := range rows {
			if row.Payload == "" {
				continue
			}
			var payload pilotPayload
			if jerr := json.Unmarshal([]byte(row.Payload), &payload); jerr != nil || payload.Profile.Name == "" {
				continue
			}
			if _, ok := nameMatchTier(payload.Profile.Name, q); !ok {
				continue
			}
			items = append(items, kbSearchItem{Kind: "character", ID: row.CharacterID, Name: payload.Profile.Name, Sub: payload.Corp.Name})
		}
		charHit = len(items) > 0
		add(items)
	} else {
		logging.Errorf("killboard search: pilot records for %q: %v", q, err)
	}
	if rows, err := app.queries.SearchCorporationRecordsByName(ctx, q); err == nil {
		var items []kbSearchItem
		for _, row := range rows {
			payload, ok := decodeCorpPayload(row.Payload)
			if !ok {
				continue
			}
			if _, ok := nameMatchTier(payload.Corp.Name, q); !ok {
				continue
			}
			sub := ""
			if payload.Corp.Ticker != "" {
				sub = "[" + payload.Corp.Ticker + "]"
			}
			items = append(items, kbSearchItem{Kind: "corporation", ID: row.CorporationID, Name: payload.Corp.Name, Sub: sub})
		}
		add(items)
	} else {
		logging.Errorf("killboard search: corporation records for %q: %v", q, err)
	}
	if rows, err := app.queries.SearchAllianceRecordsByName(ctx, q); err == nil {
		var items []kbSearchItem
		for _, row := range rows {
			payload, ok := decodeAlliancePayload(row.Payload)
			if !ok {
				continue
			}
			if _, ok := nameMatchTier(payload.Alliance.Name, q); !ok {
				continue
			}
			sub := ""
			if payload.Alliance.Ticker != "" {
				sub = "[" + payload.Alliance.Ticker + "]"
			}
			items = append(items, kbSearchItem{Kind: "alliance", ID: row.AllianceID, Name: payload.Alliance.Name, Sub: sub})
		}
		add(items)
	} else {
		logging.Errorf("killboard search: alliance records for %q: %v", q, err)
	}

	// No warmed pilot matched: a signed-in search for a full pilot
	// name notes the resolution want, exactly as the banner search
	// does, and a name resolved a moment ago answers at once.
	if userID > 0 && !charHit {
		p, hit := app.notePilotNameSearch(ctx, q, shipHits)
		pending = p
		if hit != nil {
			tier, _ := nameMatchTier(hit.Name, q)
			cands = append(cands, kbSearchCand{item: kbSearchItem{Kind: "character", ID: hit.ID, Name: hit.Name}, tier: tier})
		}
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].tier != cands[j].tier {
			return cands[i].tier < cands[j].tier
		}
		return strings.ToLower(cands[i].item.Name) < strings.ToLower(cands[j].item.Name)
	})
	for _, c := range cands {
		hits = append(hits, c.item)
	}
	if len(hits) > kbSearchLimit {
		hits = hits[:kbSearchLimit]
	}
	return hits, pending
}

// handleKillboardSuggest serves GET /intel/kills/suggest?q=: the
// killboard box's live suggestions. Local tables only, like every
// search feed in the app. While a pilot name resolves, the answer
// carries the pending row and is never cached, so the box's next
// ask sees the real suggestion when it lands.
func (app *Application) handleKillboardSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := searchQuery(r)
	items := []kbSearchItem{}
	pending := false
	if len(q) >= 2 {
		var hits []kbSearchItem
		hits, pending = app.killboardSearch(ctx, q, app.userID(ctx))
		items = append(items, hits...)
	}
	if pending {
		items = append(items, kbSearchItem{Kind: "pilot-pending", Name: "Searching for “" + q + "”…"})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if pending {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=30")
	}
	_ = json.NewEncoder(w).Encode(items)
}
