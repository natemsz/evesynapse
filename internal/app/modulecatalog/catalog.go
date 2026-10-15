// Package modulecatalog lists every optional module the application has,
// in registration (nav) order. Only the module build tool (cmd/modbuild)
// imports it: the application itself imports just the modules a build
// selects (internal/app/modules_enabled.go), so listing a module here
// compiles nothing into a build that did not choose it.
package modulecatalog

import (
	"evesynapse/internal/app/buildplanner"
	"evesynapse/internal/app/calendar"
	"evesynapse/internal/app/contacts"
	"evesynapse/internal/app/contracts"
	"evesynapse/internal/app/corpdata"
	"evesynapse/internal/app/discord"
	"evesynapse/internal/app/doctrines"
	"evesynapse/internal/app/fitting"
	"evesynapse/internal/app/groups"
	"evesynapse/internal/app/intel"
	"evesynapse/internal/app/leaderboard"
	"evesynapse/internal/app/mail"
	"evesynapse/internal/app/module"
	"evesynapse/internal/app/orders"
	"evesynapse/internal/app/piplanner"
	"evesynapse/internal/app/publicfits"
	"evesynapse/internal/app/restock"
	"evesynapse/internal/app/scanner"
	"evesynapse/internal/app/skillplans"
	"evesynapse/internal/app/srp"
	"evesynapse/internal/app/swarm"
	"evesynapse/internal/app/tradefinder"
	"evesynapse/internal/app/tradeideas"
	"evesynapse/internal/app/wallet"
)

// Entry is one module: the package that holds it and its declaration.
type Entry struct {
	// Pkg is the package name and the last element of its import path
	// (evesynapse/internal/app/<Pkg>).
	Pkg    string
	Module module.Module
}

// All is every module, in the order they register: a module whose menu
// entry sits after another module’s must come later.
var All = []Entry{
	{"buildplanner", buildplanner.Module},
	{"fitting", fitting.Module},
	{"publicfits", publicfits.Module},
	{"skillplans", skillplans.Module},
	{"wallet", wallet.Module},
	{"mail", mail.Module},
	{"calendar", calendar.Module},
	{"contacts", contacts.Module},
	{"scanner", scanner.Module},
	{"tradefinder", tradefinder.Module},
	{"leaderboard", leaderboard.Module},
	{"restock", restock.Module},
	{"orders", orders.Module},
	{"contracts", contracts.Module},
	{"corpdata", corpdata.Module},
	{"doctrines", doctrines.Module},
	{"srp", srp.Module},
	{"groups", groups.Module},
	{"discord", discord.Module},
	{"swarm", swarm.Module},
	{"intel", intel.Module},
	{"piplanner", piplanner.Module},
	{"tradeideas", tradeideas.Module},
}

// ByID returns the entry for a module id.
func ByID(id string) (Entry, bool) {
	for _, e := range All {
		if e.Module.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}
