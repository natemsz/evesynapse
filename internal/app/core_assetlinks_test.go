package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAssetLinks: the site names its Android app only when both the
// package and a certificate fingerprint are set, and writes the
// fingerprints the way Android compares them.
func TestAssetLinks(t *testing.T) {
	f := newNotifyFixture(t)
	get := func() (int, string) {
		rec := httptest.NewRecorder()
		f.app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, assetLinksPath, nil))
		return rec.Code, rec.Body.String()
	}
	if code, _ := get(); code != http.StatusNotFound {
		t.Fatalf("with no app set up: status %d, want 404", code)
	}
	lower := strings.Repeat("ab", 32)
	f.app.cfg.androidPackage = "app.evesynapse.twa"
	f.app.cfg.androidFingerprints = parseAndroidFingerprints(lower + ", nonsense, sha256 Fingerprint=" + strings.Repeat("0A:", 31) + "0A")
	code, body := get()
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	mustContain(t, assetLinksPath, body, `"package_name":"app.evesynapse.twa"`, "delegate_permission/common.handle_all_urls",
		strings.Repeat("AB:", 31)+"AB", strings.Repeat("0A:", 31)+"0A")
	if strings.Contains(body, "nonsense") {
		t.Fatal("something that is not a fingerprint was published")
	}
	f.app.cfg.androidPackage = "not a package"
	if code, _ := get(); code != http.StatusNotFound {
		t.Fatalf("with a bad package name: status %d, want 404", code)
	}
}
