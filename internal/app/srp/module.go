// Package srp is the Ship Replacement module (/srp/ and each fleet's
// tab): pilots file a loss with the corporation, alliance or community
// that ran the fleet, and its handlers pay or deny it, with the pay list
// mailed in game. The request tables and the permission model are core's;
// the notification source for new and answered requests lives with
// core's notifier (corp_srp_notify.go).
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
	NotifyKinds: []module.NotifyKindDecl{
		{ID: "srp_new", Title: "New SRP request to handle", Account: true, After: "op_reminder"},
		{ID: "srp_done", Title: "SRP request paid or denied", After: "srp_new"},
	},
	Activities: []string{"corporation"},
	Nav: []module.NavSection{{
		Section: "corporation",
		Entries: []module.NavEntry{
			{Href: "/srp/", Label: "Ship Replacement", Pages: []string{"srp.html"}, After: "/doctrines/fits/"},
		},
	}, {
		Section: "fleets",
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
		auth.Post("/srp/policies", h((*mod).handleSRPPolicySave))
		auth.Post("/srp/policies/{id}/delete", h((*mod).handleSRPPolicyDelete))

		// A pilot with no account claims from the fleet's own tab.
		anon := r.With(deps.AnonPages())
		anon.Get("/fleets/{opID:[0-9]+}/srp", h((*mod).handleFleetSRP))
		anon.Post("/fleets/{opID:[0-9]+}/srp/claim", h((*mod).handleFleetSRPClaim))
		anon.Post("/fleets/{opID:[0-9]+}/srp/{requestID:[0-9]+}/withdraw", h((*mod).handleFleetSRPWithdraw))
	},
	Pages: []module.Page{
		{Template: "srp.html", Section: "fleets"},
		{Template: "fleet_srp.html", Section: "fleets"},
	},
}

// PageData is the view model passed to srp.html.
type PageData struct {
	module.ShellData
	SRP      *srpView
	FleetSRP *fleetSRPView
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }
