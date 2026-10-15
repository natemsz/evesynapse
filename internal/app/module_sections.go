package app

import (
	"context"
	"html/template"

	"evesynapse/internal/app/module"
	"evesynapse/internal/logging"
)

// syncSection is one block a module adds to the Sync page.
type syncSection struct {
	Anchor string
	Title  string
	HTML   template.HTML
}

// moduleSections renders the page sections every registered module
// contributes; a section that renders nothing is left out. each, when
// given, is told of every one rendered (the sync page keeps its anchor
// and title); the returned list is the bare HTML.
func (app *Application) moduleSections(ctx context.Context, pick func(module.Module) []module.PageSection, each func(module.PageSection, template.HTML)) []template.HTML {
	var out []template.HTML
	for _, m := range registeredModules {
		for _, sec := range pick(m) {
			html := func() (html template.HTML) {
				defer func() {
					if r := recover(); r != nil {
						logging.Errorf("module %s: section %q: %v", m.ID, sec.Title, r)
						html = ""
					}
				}()
				return sec.Render(ctx, app)
			}()
			if html == "" {
				continue
			}
			out = append(out, html)
			if each != nil {
				each(sec, html)
			}
		}
	}
	return out
}
