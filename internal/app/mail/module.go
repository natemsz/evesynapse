// Package mail is the Mail module: the character mail pages: inbox, reading, composing and sending in game. It reads the
// snapshots core keeps fetched; the fetch itself is core's.
package mail

import (
	"evesynapse/internal/app/module"
	"evesynapse/internal/esi"
	"net/http"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
)

// Module is the Mail module's registration.
var Module = module.Module{
	ID:       "mail",
	Title:    "Mail",
	Requires: []string{},
	// This is the feature's own declaration: the same scopes core's manifest
	// names for the data feed it keeps fetched (a test pins that they agree).
	Scopes: []module.Scope{
		{Scope: "esi-mail.read_mail.v1", Unlocks: "Inbox, labels, mailing lists and bodies."},
	},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-mail.organize_mail.v1", Unlocks: "Mark mail as read."},
		{Scope: "esi-mail.send_mail.v1", Unlocks: "Compose and send mail."},
	},
	Snapshots: []string{esi.SnapMail, esi.SnapMailLabels, esi.SnapMailLists, esi.SnapMailBodyPrefix},
	Nav: []module.NavSection{{
		Section: "pilot",
		Entries: []module.NavEntry{
			{Href: "/mail/", Label: "Mail", Pages: []string{"mail.html"}, After: "/wallet/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/mail/", h((*mod).handleMail))
		auth.Post("/mail/read/", h((*mod).handleMailMarkRead))
		auth.Get("/mail/compose/", h((*mod).handleMailCompose))
		auth.Post("/mail/send/", h((*mod).handleMailSend))
	},
	Pages: []module.Page{
		{Template: "mail.html", Section: "pilot"},
		{Template: "mail_compose.html", Section: "pilot"},
	},
}

// PageData is the view model passed to the Mail templates.
type PageData struct {
	module.ShellData
	Mail        *mailView
	MailChars   []module.CharLink
	MailCompose *mailComposeView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }
