//go:build !core_only

package app

import (
	"evesynapse/internal/app/intel"
	"evesynapse/internal/app/module"
	"evesynapse/internal/app/piplanner"
)

// The build-selected modules. A module is compiled in when this file
// imports its package, and left out when it does not: Go excludes what
// is never imported. The default build (this file) enables every
// module; `go build -tags core_only` swaps it for modules_core_only.go
// and enables none. One line per module, in nav order. The module
// build tool (stage S14) writes this file for a chosen selection.
func init() {
	registerModules([]module.Module{
		intel.Module,
		piplanner.Module,
	})
}
