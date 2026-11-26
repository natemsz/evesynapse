// Command evesynapse-dev is the local-development entrypoint for
// EveSynapse. It wires the application exactly like the release
// entrypoint (cmd/evesynapse) and additionally registers the
// dev-only routes from internal/devtools (today: /dev-login, only
// active when DEV_LOGIN=1).
//
// !!! Local testing only. /dev-login hands out a signed-in session
// to anyone who asks; never deploy this binary anywhere anyone else
// can reach. Forkers and production deployments should use
// cmd/evesynapse, which contains no devtools code at all. !!!
//
// The release entrypoint's maintenance modes work here too
// (internal/appmain); -update installs over this dev binary and the
// restart hand-off targets whichever server wrote the pidfile.
package main

import (
	"os"

	"evesynapse/internal/app"
	"evesynapse/internal/appmain"
	"evesynapse/internal/devtools"
)

func main() {
	os.Exit(appmain.Run("evesynapse-dev", os.Args[1:], os.Stdout, os.Stderr, appmain.Hooks{
		Prepare: devtools.Prepare,
		Routes:  []app.RouteHook{devtools.Register},
		RunNote: " (with dev routes)",
	}))
}
