package app

// ---------------------------------------------------------------------------
// The intel classifier on the site: the tagger (internal/inteltag)
// reading reports beside the deterministic analysis.
//
// The model is embedded in the binary; INTEL_TAGGER_WEIGHTS points
// the site at another model file to try, and INTEL_TAGGER=off reads
// without it. The tagger's name lists are the site's own: the SDE's
// systems, ships (with the nicknames scouts use), regions and
// constellations, and the pilot names EVE has confirmed (the same
// names a fused roster is segmented against).
//
// What it changes is narrow on purpose: its pilot runs order the
// analysis's pilot pass (intelanalyze.Dict.Names), so names typed
// with single spaces split where they should. Every name is still
// looked up, and every rule of the deterministic reading still
// holds; with the tagger off or failed to load, reading is exactly
// as before.
// ---------------------------------------------------------------------------

import (
	"context"
	"os"
	"strings"
	"sync"

	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/inteltag"
	"evesynapse/internal/logging"
)

type intelTaggerHolder struct {
	once  sync.Once
	model *inteltag.Model
}

var intelTaggers sync.Map // *Application -> *intelTaggerHolder

// intelTagger is the loaded model, or nil when it is off or did not
// load (logged once).
func (app *Application) intelTagger() *inteltag.Model {
	if app.cfg.intelTaggerOff {
		return nil
	}
	v, _ := intelTaggers.LoadOrStore(app, &intelTaggerHolder{})
	h := v.(*intelTaggerHolder)
	h.once.Do(func() {
		var m *inteltag.Model
		var err error
		if p := app.cfg.intelTaggerPath; p != "" {
			var f *os.File
			if f, err = os.Open(p); err == nil {
				m, err = inteltag.Load(f)
				f.Close()
			}
		} else {
			m, err = inteltag.Default()
		}
		if err != nil {
			logging.Warnf("intel: classifier not loaded, reading without it: %v", err)
			return
		}
		h.model = m
	})
	return h.model
}

// siteLexicon answers the tagger's name lists from the site's
// dictionaries.
type siteLexicon struct {
	systems func(lower string) (int64, string, bool)
	ships   map[string]intelShip
	areas   map[string]int64
	pilots  map[string]bool
	nicks   map[string]string
}

func (l siteLexicon) Kinds(p string) []string {
	var out []string
	if _, _, ok := l.systems(p); ok {
		out = append(out, "system")
	}
	for _, c := range []string{p, strings.TrimSuffix(p, "s")} {
		if _, ok := l.ships[c]; ok {
			out = append(out, "ship")
			break
		}
		if full, ok := l.nicks[c]; ok {
			if _, ok := l.ships[full]; ok {
				out = append(out, "ship")
				break
			}
		}
	}
	if _, ok := l.areas[p]; ok {
		out = append(out, "area")
	}
	if l.pilots[p] {
		out = append(out, "pilot")
	}
	return out
}

var intelShipNicks = intelanalyze.ShipNicknames()

// intelNamesHook is the analysis's Names hook for one dictionary,
// or nil when the classifier is off.
func (app *Application) intelNamesHook(ctx context.Context, ships map[string]intelShip, systems func(string) (int64, string, bool), areas map[string]int64) func(string) []string {
	m := app.intelTagger()
	if m == nil {
		return nil
	}
	lex := siteLexicon{systems: systems, ships: ships, areas: areas, pilots: app.intelKnownNames(ctx), nicks: intelShipNicks}
	return func(text string) []string {
		var out []string
		for _, sp := range m.TagWith(text, lex) {
			if sp.Kind == "pilot" {
				out = append(out, text[sp.Start:sp.End])
			}
		}
		return out
	}
}
