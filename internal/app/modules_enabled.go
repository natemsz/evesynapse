package app

import (
	"evesynapse/internal/app/module"
	"evesynapse/internal/app/piplanner"
)

// enabledModules lists the build-selected modules compiled into the binary.
var enabledModules = []module.Module{
	piplanner.Module,
}

func init() {
	registerModules(enabledModules)
}
