package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	json "github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/tags"
)

// ---------------------------------------------------------------------------
// Home widget settings. A widget declares its options (homeWidgetDef.
// Options); the account's values for them are one JSON object of option
// values per (user, layout key) in widget_configs, so they survive
// layout saves and a widget being removed and added again. A value is
// used only while it is still valid for the account: a choice the
// widget offers, a character still linked, a tag still in use. Anything
// else reads as the option's default. The framework applies the scope
// option itself, narrowing a builder's characters and bundles and
// naming the scope in the card's title.
// ---------------------------------------------------------------------------

// Scope option values: every character, one character, or the
// characters carrying a tag.
const (
	scopeAll     = "all"
	scopeCharPfx = "char:"
	scopeTagPfx  = "tag:"
)

// Toggle option values.
const (
	toggleOn  = "yes"
	toggleOff = "no"
)

// widgetScope is the scope a card draws for.
type widgetScope struct {
	Value string // "all", "char:<id>" or "tag:<name>"
	Char  string // the character's name, for a one-character scope
	Tag   string // the tag, for a tag scope
}

// All reports whether the scope covers every character.
func (s widgetScope) All() bool { return s.Char == "" && s.Tag == "" }

// Label names the scope for a card's title: "" for every character.
func (s widgetScope) Label() string {
	switch {
	case s.Char != "":
		return s.Char
	case s.Tag != "":
		return "Tag: " + s.Tag
	}
	return ""
}

// userTags lists every tag in use across the account's characters.
func userTags(chars []db.Character) []string { return tags.InUse(chars) }

// scopeOnAccount reports whether a scope value names this account's
// characters: every character, a linked character, or a tag in use.
func scopeOnAccount(v string, chars []db.Character) bool {
	switch {
	case v == scopeAll:
		return true
	case strings.HasPrefix(v, scopeCharPfx):
		id, err := strconv.ParseInt(strings.TrimPrefix(v, scopeCharPfx), 10, 64)
		if err != nil {
			return false
		}
		for _, c := range chars {
			if c.CharacterID == id {
				return true
			}
		}
	case strings.HasPrefix(v, scopeTagPfx):
		tag := strings.TrimPrefix(v, scopeTagPfx)
		for _, t := range userTags(chars) {
			if t == tag {
				return true
			}
		}
	}
	return false
}

// applyScope narrows the characters and their bundles to a scope value
// already checked against the account.
func applyScope(v string, chars []db.Character, bundles []*charSnaps) ([]db.Character, []*charSnaps, widgetScope) {
	scope := widgetScope{Value: scopeAll}
	var keep func(db.Character) bool
	switch {
	case strings.HasPrefix(v, scopeCharPfx):
		id, _ := strconv.ParseInt(strings.TrimPrefix(v, scopeCharPfx), 10, 64)
		keep = func(c db.Character) bool { return c.CharacterID == id }
	case strings.HasPrefix(v, scopeTagPfx):
		tag := strings.TrimPrefix(v, scopeTagPfx)
		keep = func(c db.Character) bool {
			for _, t := range splitTags(c.Tags) {
				if t == tag {
					return true
				}
			}
			return false
		}
		scope = widgetScope{Value: v, Tag: tag}
	default:
		return chars, bundles, scope
	}
	ids := map[int64]bool{}
	var outChars []db.Character
	for _, c := range chars {
		if keep(c) {
			ids[c.CharacterID] = true
			outChars = append(outChars, c)
			if scope.Tag == "" {
				scope = widgetScope{Value: v, Char: c.Name}
			}
		}
	}
	var outBundles []*charSnaps
	for _, b := range bundles {
		if ids[b.ch.CharacterID] {
			outBundles = append(outBundles, b)
		}
	}
	return outChars, outBundles, scope
}

// valid reports whether v is a value option o may hold for this
// account.
func (o widgetOption) valid(v string, chars []db.Character) bool {
	if !o.allows(v) {
		return false
	}
	return o.Kind != optKindScope || scopeOnAccount(v, chars)
}

// scopeOption is the widget's scope option, when it declares one.
func (d *homeWidgetDef) scopeOption() (widgetOption, bool) {
	for _, o := range d.Options {
		if o.Kind == optKindScope {
			return o, true
		}
	}
	return widgetOption{}, false
}

// optionsFrom reads a stored settings object: each declared option at
// its stored value while that value is valid for the account, at its
// default otherwise. Keys the widget does not declare are dropped, and
// a blob that is not an object reads as every default.
func (d *homeWidgetDef) optionsFrom(blob string, chars []db.Character) widgetOptions {
	out := d.defaultOptions()
	if strings.TrimSpace(blob) == "" {
		return out
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &raw); err != nil {
		return out
	}
	for _, o := range d.Options {
		var v string
		if r, ok := raw[o.Key]; ok && json.Unmarshal(r, &v) == nil && o.valid(v, chars) {
			out[o.Key] = v
		}
	}
	return out
}

// Config is the stored settings object for a layout key ("" when the
// account has none). Every configuration is read in one query, the
// first time any card asks.
func (s *homeShared) Config(ctx context.Context, key string) string {
	if s.configs == nil {
		s.configs = map[string]string{}
		rows, err := s.app.queries.ListWidgetConfigs(ctx, s.userID)
		if err != nil {
			logging.Errorf("home: widget settings for user %d: %v", s.userID, err)
		}
		for _, r := range rows {
			s.configs[r.WidgetID] = r.Config
		}
	}
	return s.configs[key]
}

// widgetSettingsView is a widget's settings sheet: a plain form that
// posts to /home/widget-config. Customize puts it in each card (the
// gear opens it in the shared modal) and in the no-script module list.
type widgetSettingsView struct {
	Key, Title string
	Fields     []widgetSettingsField
}

// widgetSettingsField is one option as the sheet draws it.
type widgetSettingsField struct {
	Key, Label, Kind, Hint string
	Choices                []widgetSettingsChoice // scope, choice and multi
	On                     bool                   // toggle
	Max                    int                    // multi: the most that may be picked, 0 for any number
}

// widgetSettingsChoice is one value a field offers.
type widgetSettingsChoice struct {
	Value, Label string
	Selected     bool
}

// settingsView is the sheet for a widget at its current values; nil
// when the widget declares no options. A scope field offers every
// character, then every tag in use.
func (d *homeWidgetDef) settingsView(key string, opts widgetOptions, chars []db.Character) *widgetSettingsView {
	if len(d.Options) == 0 {
		return nil
	}
	v := &widgetSettingsView{Key: key, Title: d.Title}
	for _, o := range d.Options {
		cur := opts.Get(o.Key)
		f := widgetSettingsField{Key: o.Key, Label: o.Label, Kind: o.Kind, Hint: o.Hint, Max: o.Max, On: cur == toggleOn}
		switch o.Kind {
		case optKindScope:
			f.Choices = append(f.Choices, widgetSettingsChoice{Value: scopeAll, Label: "All characters", Selected: cur == scopeAll})
			for _, c := range chars {
				val := scopeCharPfx + strconv.FormatInt(c.CharacterID, 10)
				f.Choices = append(f.Choices, widgetSettingsChoice{Value: val, Label: c.Name, Selected: cur == val})
			}
			for _, t := range userTags(chars) {
				val := scopeTagPfx + t
				f.Choices = append(f.Choices, widgetSettingsChoice{Value: val, Label: "Tag: " + t, Selected: cur == val})
			}
		case optKindChoice, optKindMulti:
			picked := map[string]bool{cur: true}
			if o.Kind == optKindMulti {
				picked = map[string]bool{}
				for _, p := range strings.Split(cur, ",") {
					picked[p] = true
				}
			}
			for _, c := range o.Choices {
				f.Choices = append(f.Choices, widgetSettingsChoice{Value: c.Value, Label: c.Label, Selected: picked[c.Value]})
			}
		}
		v.Fields = append(v.Fields, f)
	}
	return v
}

// formOptions overlays a settings form on the stored values: each
// option the form carries takes its posted value, and an option the
// form leaves out keeps its stored value. ok is false when a posted
// value is not valid for the account (too many picks or none, a choice
// not offered, another account's character): the caller then keeps
// what is stored. A toggle posts "no" from a hidden field ahead of its
// checkbox, so its last value is the one that counts; a multi posts
// one value per pick behind an empty marker.
func (d *homeWidgetDef) formOptions(form map[string][]string, stored widgetOptions, chars []db.Character) (opts widgetOptions, ok bool) {
	out := make(widgetOptions, len(stored))
	for k, v := range stored {
		out[k] = v
	}
	for _, o := range d.Options {
		vals, ok := form[o.Key]
		if !ok || len(vals) == 0 {
			continue
		}
		var v string
		switch o.Kind {
		case optKindToggle:
			v = vals[len(vals)-1]
		case optKindMulti:
			var picks []string
			for _, p := range vals {
				if p != "" {
					picks = append(picks, p)
				}
			}
			v = strings.Join(picks, ",")
		default:
			v = vals[0]
		}
		if !o.valid(v, chars) {
			return stored, false
		}
		out[o.Key] = v
	}
	return out, true
}

// storedOptions is the account's stored settings for one layout key.
func (app *Application) storedOptions(ctx context.Context, userID int64, def *homeWidgetDef, key string, chars []db.Character) widgetOptions {
	blob, err := app.queries.GetWidgetConfig(ctx, db.GetWidgetConfigParams{UserID: userID, WidgetID: key})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		logging.Errorf("home: widget settings %s for user %d: %v", key, userID, err)
	}
	return def.optionsFrom(blob, chars)
}

// handleWidgetConfig saves one widget's settings (POST
// /home/widget-config): key names the card, the option fields follow.
// Values are checked against the account: a stale or hand-made form
// is refused and the stored settings stand. Scripted callers get
// {"ok":true} and refetch the card, or a 400 when nothing was saved;
// plain forms go back to Customize (or to next, when it is a local
// path).
func (app *Application) handleWidgetConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	next := "/?customize=1"
	saved := false
	if err := r.ParseForm(); err == nil {
		if n := r.PostForm.Get("next"); safeLocalNext(n) {
			next = n
		}
		key := r.PostForm.Get("key")
		if def, ok := widgetDefForKey(key); ok && len(def.Options) > 0 {
			saved = app.saveWidgetOptions(ctx, userID, def, key, r.PostForm)
		}
	}
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		w.Header().Set("Content-Type", "application/json")
		if !saved {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// saveWidgetOptions stores a settings form for one card.
func (app *Application) saveWidgetOptions(ctx context.Context, userID int64, def *homeWidgetDef, key string, form map[string][]string) bool {
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("home: widget settings: list characters for user %d: %v", userID, err)
		return false
	}
	opts, ok := def.formOptions(form, app.storedOptions(ctx, userID, def, key, chars), chars)
	if !ok {
		return false
	}
	blob, err := json.Marshal(opts)
	if err != nil {
		logging.Errorf("home: encode widget settings for user %d: %v", userID, err)
		return false
	}
	if err := app.queries.UpsertWidgetConfig(ctx, db.UpsertWidgetConfigParams{
		UserID: userID, WidgetID: key, Config: string(blob), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("home: save widget settings for user %d: %v", userID, err)
		return false
	}
	return true
}
