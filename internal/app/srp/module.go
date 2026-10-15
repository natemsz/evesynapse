// Package srp is the Ship Replacement module (/srp/): pilots file a loss
// for their corporation's replacement programme and its handlers pay or
// deny it, with the pay list mailed in game. The request tables and the
// permission model are core's; the notification source for new and
// answered requests lives with core's notifier (corp_srp_notify.go).
package srp

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
)

// Module is the Ship Replacement module's registration.
var Module = module.Module{
	ID:       "srp",
	Title:    "Ship Replacement",
	Requires: []string{},
	// Anyone in a corporation can file a loss; who handles requests is the
	// corporation's own permission rule.
	Scopes: []module.Scope{},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-killmails.read_killmails.v1", Unlocks: "A pilot's recent losses are offered to claim."},
		{Scope: "esi-mail.send_mail.v1", Unlocks: "Handlers mail the pay list and tell a pilot their request was paid."},
		{Scope: "esi-characters.read_corporation_roles.v1", Unlocks: "Directors and role holders can handle their corporation's requests."},
	},
	Nav: []module.NavSection{{
		Section: "corporation",
		Entries: []module.NavEntry{
			{Href: "/srp/", Label: "Ship Replacement", Pages: []string{"srp.html"}, After: "/doctrines/fits/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/srp/", h((*mod).handleSRP))
		auth.Post("/srp/request", h((*mod).handleSRPRequest))
		auth.Post("/srp/paylist", h((*mod).handleSRPPayList))
		auth.Post("/srp/{id}/decide", h((*mod).handleSRPDecide))
		auth.Post("/srp/{id}/withdraw", h((*mod).handleSRPWithdraw))
	},
	Pages: []module.Page{
		{Template: "srp.html", Section: "corporation"},
	},
}

// PageData is the view model passed to srp.html.
type PageData struct {
	module.ShellData
	SRP *srpView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }
