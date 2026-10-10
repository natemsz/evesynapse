package app

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Digital Asset Links, for the Android wrapper (android/).
//
// The Android app is a Trusted Web Activity: a thin wrapper that opens
// this site in the phone's browser without the address bar. The
// browser only hides the bar, and only lets the app speak for the
// site's notifications, when the site names the app here: its package
// and the fingerprint of the certificate it is signed with. Both are
// public, and the statement gives the app nothing but that.
//
// ANDROID_APP_PACKAGE and ANDROID_APP_FINGERPRINTS set it; with either
// unset there is no statement and the address answers 404.
// ---------------------------------------------------------------------------

const assetLinksPath = "/.well-known/assetlinks.json"

var (
	androidPackageRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	hexOnlyRe        = regexp.MustCompile(`^[0-9A-F]{64}$`)
)

// parseAndroidFingerprints reads a comma-separated list of SHA-256
// certificate fingerprints, with or without colons, into the form
// Android compares: upper-case pairs joined by colons. What is not a
// fingerprint is left out, with a warning.
func parseAndroidFingerprints(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// OpenSSL prints "sha256 Fingerprint=AB:CD:…"; the part after "=" is it.
		if i := strings.LastIndex(part, "="); i >= 0 {
			part = part[i+1:]
		}
		hex := strings.ToUpper(strings.ReplaceAll(part, ":", ""))
		if !hexOnlyRe.MatchString(hex) {
			logging.Warnf("evesynapse: ANDROID_APP_FINGERPRINTS: %q is not a SHA-256 fingerprint; left out", part)
			continue
		}
		pairs := make([]string, 0, 32)
		for i := 0; i < len(hex); i += 2 {
			pairs = append(pairs, hex[i:i+2])
		}
		out = append(out, strings.Join(pairs, ":"))
	}
	return out
}

// assetLinks is the statement naming the Android app, or nil when the
// app is not set up.
func (c Config) assetLinks() []byte {
	if !androidPackageRe.MatchString(c.androidPackage) || len(c.androidFingerprints) == 0 {
		return nil
	}
	body, err := json.Marshal([]map[string]any{{
		"relation": []string{"delegate_permission/common.handle_all_urls"},
		"target": map[string]any{
			"namespace":                "android_app",
			"package_name":             c.androidPackage,
			"sha256_cert_fingerprints": c.androidFingerprints,
		},
	}})
	if err != nil {
		return nil
	}
	return body
}

func (app *Application) handleAssetLinks(w http.ResponseWriter, r *http.Request) {
	body := app.cfg.assetLinks()
	if body == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(body)
}
