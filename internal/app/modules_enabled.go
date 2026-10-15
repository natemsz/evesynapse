//go:build !core_only

package app

import (
	"evesynapse/internal/app/buildplanner"
	"evesynapse/internal/app/corpdata"
	"evesynapse/internal/app/discord"
	"evesynapse/internal/app/doctrines"
	"evesynapse/internal/app/fitting"
	"evesynapse/internal/app/groups"
	"evesynapse/internal/app/intel"
	"evesynapse/internal/app/leaderboard"
	"evesynapse/internal/app/module"
	"evesynapse/internal/app/piplanner"
	"evesynapse/internal/app/publicfits"
	"evesynapse/internal/app/restock"
	"evesynapse/internal/app/scanner"
	"evesynapse/internal/app/skillplans"
	"evesynapse/internal/app/srp"
	"evesynapse/internal/app/tradefinder"
	"evesynapse/internal/app/tradeideas"
)

// The build-selected modules. A module is compiled in when this file
// imports its package, and left out when it does not: Go excludes what
// is never imported. The default build (this file) enables every
// module; `go build -tags core_only` swaps it for modules_core_only.go
// and enables none. One line per module, in nav order. The module
// build tool (stage S14) writes this file for a chosen selection.
func init() {
	registerModules([]module.Module{
		buildplanner.Module,
		fitting.Module,
		publicfits.Module,
		skillplans.Module,
		scanner.Module,
		tradefinder.Module,
		leaderboard.Module,
		restock.Module,
		corpdata.Module,
		doctrines.Module,
		srp.Module,
		groups.Module,
		discord.Module,
		intel.Module,
		piplanner.Module,
		tradeideas.Module,
	})
}
