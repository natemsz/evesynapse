package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// v0.3.04: per-widget configuration (schema 020). The home
// layout decides which widgets show and in what order; this
// decides what a widget does. Config is one JSON object per
// (user, widget id), owned by the widget: it survives layout
// saves, module removal and re-adding, because the widget id is
// the stable instance identity (a layout holds each id at most
// once). The orders widget is the first tenant — its scope
// (all characters / one character / one tag) and merge mode
// (combined totals, per-character rows, or both).
// ---------------------------------------------------------------------------

// Orders-widget scope values.
const (
	scopeAll       = "all"
	scopeCharacter = "character"
	scopeTag       = "tag"
)

// Orders-widget merge modes: how a multi-character scope reads.
const (
	mergeBoth         = "both"          // totals header + per-character rows
	mergeCombined     = "combined"      // totals only
	mergePerCharacter = "per-character" // per-character rows only
)

// ordersWidgetConfig is the parsed configuration of the Market
// orders widget. The zero value (and any missing/blank stored
// config) means the defaults: all characters, merged both ways.
type ordersWidgetConfig struct {
	ScopeType   string `json:"scope_type"` // scopeAll | scopeCharacter | scopeTag
	CharacterID int64  `json:"character_id,omitempty"`
	Tag         string `json:"tag,omitempty"`
	Merge       string `json:"merge"` // mergeBoth | mergeCombined | mergePerCharacter
}

// defaultOrdersWidgetConfig is what an unconfigured widget does.
func defaultOrdersWidgetConfig() ordersWidgetConfig {
	return ordersWidgetConfig{ScopeType: scopeAll, Merge: mergeBoth}
}

// parseOrdersWidgetConfig decodes a stored blob, normalizing
// every field back to the default when it is missing or junk —
// a corrupted config must degrade to the default scope, never
// break the home.
func parseOrdersWidgetConfig(blob string) ordersWidgetConfig {
	cfg := defaultOrdersWidgetConfig()
	if strings.TrimSpace(blob) == "" {
		return cfg
	}
	var raw ordersWidgetConfig
	if err := json.Unmarshal([]byte(blob), &raw); err != nil {
		return cfg
	}
	switch raw.ScopeType {
	case scopeCharacter:
		if raw.CharacterID > 0 {
			cfg.ScopeType, cfg.CharacterID = scopeCharacter, raw.CharacterID
		}
	case scopeTag:
		if raw.Tag != "" {
			cfg.ScopeType, cfg.Tag = scopeTag, raw.Tag
		}
	case scopeAll:
	}
	switch raw.Merge {
	case mergeBoth, mergeCombined, mergePerCharacter:
		cfg.Merge = raw.Merge
	}
	return cfg
}

// encoded names one scope choice as the forms carry it:
// "all", "char:<id>", or "tag:<name>".
func (c ordersWidgetConfig) encoded() string {
	switch c.ScopeType {
	case scopeCharacter:
		return "char:" + strconv.FormatInt(c.CharacterID, 10)
	case scopeTag:
		return "tag:" + c.Tag
	}
	return scopeAll
}

// decodeScopeValue splits a form's scope choice back into its
// parts; anything unrecognized is the all-characters scope.
func decodeScopeValue(raw string) ordersWidgetConfig {
	cfg := defaultOrdersWidgetConfig()
	switch {
	case strings.HasPrefix(raw, "char:"):
		if id, err := strconv.ParseInt(strings.TrimPrefix(raw, "char:"), 10, 64); err == nil && id > 0 {
			cfg.ScopeType, cfg.CharacterID = scopeCharacter, id
		}
	case strings.HasPrefix(raw, "tag:"):
		if tag := strings.TrimPrefix(raw, "tag:"); tag != "" {
			cfg.ScopeType, cfg.Tag = scopeTag, tag
		}
	}
	return cfg
}

// ordersConfigFor reads the account's stored orders-widget
// config (defaults when none was ever saved).
func (app *Application) ordersConfigFor(ctx context.Context, userID int64) ordersWidgetConfig {
	blob, err := app.queries.GetWidgetConfig(ctx, db.GetWidgetConfigParams{
		UserID: userID, WidgetID: widgetMarket,
	})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("home: widget config for user %d: %v", userID, err)
		}
		return defaultOrdersWidgetConfig()
	}
	return parseOrdersWidgetConfig(blob)
}

// saveOrdersConfig persists one orders-widget configuration.
func (app *Application) saveOrdersConfig(ctx context.Context, userID int64, cfg ordersWidgetConfig) {
	blob, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("home: encode widget config for user %d: %v", userID, err)
		return
	}
	if err := app.queries.UpsertWidgetConfig(ctx, db.UpsertWidgetConfigParams{
		UserID:    userID,
		WidgetID:  widgetMarket,
		Config:    string(blob),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		log.Printf("home: save widget config for user %d: %v", userID, err)
	}
}

// userTags lists every tag in use across the account's
// characters (the fleet widget's chips, shared vocabulary).
func userTags(chars []db.Character) []string {
	seen := map[string]bool{}
	for _, ch := range chars {
		for _, tag := range splitTags(ch.Tags) {
			seen[tag] = true
		}
	}
	tags := make([]string, 0, len(seen))
	for tag := range seen {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

// handleWidgetConfig saves one widget's settings (POST
// /home/widget-config, today only the orders widget's scope and
// merge mode). The choices are validated against the account —
// a character must be linked, a tag must be in use — so a stale
// or hand-crafted form falls back to the all-characters scope
// instead of stranding the widget on nothing. Like the layout
// handler, XHR callers get a bare 200 and plain forms bounce
// back to where they were (the `next` field).
func (app *Application) handleWidgetConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	next := "/"
	if err := r.ParseForm(); err == nil {
		if n := r.FormValue("next"); strings.HasPrefix(n, "/") && !strings.HasPrefix(n, "//") {
			next = n
		}
		if r.FormValue("widget") == widgetMarket {
			// Overlay the stored config: a scope-only submit
			// (the merge picker hides for single-character
			// scopes) keeps the merge mode it already had.
			cfg := app.ordersConfigFor(ctx, userID)
			scoped := decodeScopeValue(r.FormValue("scope"))
			cfg.ScopeType, cfg.CharacterID, cfg.Tag = scoped.ScopeType, scoped.CharacterID, scoped.Tag
			switch r.FormValue("merge") {
			case mergeBoth, mergeCombined, mergePerCharacter:
				cfg.Merge = r.FormValue("merge")
			}

			// Validate the scope against the account: unknown
			// characters and unused tags degrade to "all".
			chars, cerr := app.queries.ListCharactersByUser(ctx, userID)
			if cerr != nil {
				log.Printf("home: widget config: list characters for user %d: %v", userID, cerr)
			} else {
				switch cfg.ScopeType {
				case scopeCharacter:
					found := false
					for _, ch := range chars {
						if ch.CharacterID == cfg.CharacterID {
							found = true
						}
					}
					if !found {
						cfg = defaultOrdersWidgetConfig()
					}
				case scopeTag:
					found := false
					for _, tag := range userTags(chars) {
						if tag == cfg.Tag {
							found = true
						}
					}
					if !found {
						cfg = defaultOrdersWidgetConfig()
					}
				}
			}
			app.saveOrdersConfig(ctx, userID, cfg)
		}
	}
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// scopeBundles selects the character bundles an orders-widget
// configuration covers, with the label the empty states speak
// in. A single-character or stale scope can select nobody; the
// caller renders the quiet line for that case.
func scopeBundles(bundles []*charSnaps, cfg ordersWidgetConfig) (scoped []*charSnaps, label string) {
	switch cfg.ScopeType {
	case scopeCharacter:
		for _, b := range bundles {
			if b.ch.CharacterID == cfg.CharacterID {
				return []*charSnaps{b}, b.ch.Name
			}
		}
		return nil, ""
	case scopeTag:
		for _, b := range bundles {
			for _, tag := range splitTags(b.ch.Tags) {
				if tag == cfg.Tag {
					scoped = append(scoped, b)
					break
				}
			}
		}
		return scoped, cfg.Tag
	default:
		return bundles, ""
	}
}
