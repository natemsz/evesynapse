// Command evesynapse is the EveSynapse web app: a personal EVE Online
// companion tool (character sheets, market, fitting, intel) backed by
// CCP's ESI API and EVE SSO.
//
// This is the release entrypoint. It intentionally does not import
// internal/devtools, so the release binary contains no dev-login
// code at all; DEV_LOGIN=1 has no effect here. For the local-dev
// build with /dev-login, see cmd/evesynapse-dev.
//
// With no arguments it runs the web app. Maintenance modes:
//
//	evesynapse -version                print the version and exit
//	evesynapse -update -arm64 / -x86   update to the latest release
//	                                   for this kind of computer
//	evesynapse -update <url> <sha256>  download, verify, and install
//	                                   a new build, restarting the
//	                                   running server onto it
//	evesynapse -push-keys              print a new key pair for browser
//	                                   push, as lines to add to .env
//	evesynapse -wrap-token-key         wrap TOKEN_ENCRYPTION_KEY with
//	                                   the OCI KMS key (oci token key
//	                                   provider setup; prints the
//	                                   TOKEN_KEY_WRAPPED ciphertext)
//	evesynapse -refresh                mark all cached data out of
//	                                   date so the next start fetches
//	                                   fresh copies (server stopped)
package main

import (
	"os"

	"evesynapse/internal/appmain"
)

func main() {
	os.Exit(appmain.Run("evesynapse", os.Args[1:], os.Stdout, os.Stderr, appmain.Hooks{}))
}
