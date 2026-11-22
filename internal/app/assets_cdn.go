package app

// ---------------------------------------------------------------------------
// The external asset CDN. ASSETS_PATH names a host that mirrors the
// /static tree. When it is set, wallpapers and landing media load
// from it, with the embedded default wallpapers and landing stills
// behind them if it can't be reached (static/assets-probe.js). When
// it is unset, the Full wallpaper option is off and every scheme's
// Muted tier shows the default muted art.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// schemeWallpaperCSS holds the per-scheme wallpaper rules. Their files
// live only on the CDN, so it stays out of the static tree.
//
//go:embed wallpapers_cdn.css
var schemeWallpaperCSS string

var assetsBaseValue atomic.Value

// assetsBase is the CDN root with no trailing slash, "" when unset.
func assetsBase() string {
	s, _ := assetsBaseValue.Load().(string)
	return s
}

func setAssetsBase(base string) { assetsBaseValue.Store(base) }

// fullWallpapers reports whether the Full wallpaper option is offered.
func fullWallpapers() bool { return assetsBase() != "" }

// parseAssetsPath normalizes ASSETS_PATH to an origin and optional
// path, adding https:// to a bare host. It returns "" for a value
// that can't be used.
func parseAssetsPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	if strings.ContainsAny(raw, " \t\r\n\"'<>;,()") {
		return ""
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	path = strings.TrimSuffix(path, "/static")
	return u.Scheme + "://" + u.Host + path
}

// assetsOrigin is the CDN's origin, for the content security policy.
func assetsOrigin() string {
	base := assetsBase()
	u, err := url.Parse(base)
	if base == "" || err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

var cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

const assetsOnScope = "html:not([data-assets-off])"

// scopeSelector makes a rule apply only while the CDN is in use.
func scopeSelector(selectors string) string {
	parts := strings.Split(selectors, ",")
	for i, s := range parts {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "html") {
			parts[i] = assetsOnScope + strings.TrimPrefix(s, "html")
		} else {
			parts[i] = assetsOnScope + " " + s
		}
	}
	return strings.Join(parts, ",")
}

// closingBrace finds the brace closing the block opened at open.
func closingBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// wallpaperRules copies a stylesheet's wallpaper rules, media queries
// included, pointed at the CDN and scoped to it. With every set, a
// rule keeps all its declarations; otherwise only background-image.
func wallpaperRules(src, base string, every bool) string {
	src = cssComment.ReplaceAllString(src, "")
	var out strings.Builder
	for len(src) > 0 {
		open := strings.IndexByte(src, '{')
		if open < 0 {
			break
		}
		end := closingBrace(src, open)
		if end < 0 {
			break
		}
		prelude, body := strings.TrimSpace(src[:open]), src[open+1:end]
		src = src[end+1:]
		switch {
		case strings.HasPrefix(prelude, "@media"):
			if inner := wallpaperRules(body, base, every); inner != "" {
				out.WriteString(prelude + "{" + inner + "}")
			}
		case strings.HasPrefix(prelude, "@"):
		case strings.Contains(body, "/static/bg"):
			var kept []string
			for _, decl := range strings.Split(body, ";") {
				decl = strings.TrimSpace(decl)
				if decl != "" && (every || strings.HasPrefix(decl, "background-image")) {
					kept = append(kept, strings.ReplaceAll(decl, `"/static/`, `"`+base+`/static/`))
				}
			}
			out.WriteString(scopeSelector(prelude) + "{" + strings.Join(kept, ";") + "}")
		}
	}
	return out.String()
}

var cdnSheets sync.Map // base + path -> []byte

// cdnStylesheet builds the stylesheets that exist only with a CDN set.
func cdnStylesheet(path string) ([]byte, bool) {
	base := assetsBase()
	if base == "" || (path != "/wallpapers-cdn.css" && path != "/landing-cdn.css") {
		return nil, false
	}
	if cached, ok := cdnSheets.Load(base + path); ok {
		return cached.([]byte), true
	}
	read := func(name string) string {
		b, _ := fs.ReadFile(staticFS, name)
		return string(b)
	}
	var sheet string
	if path == "/landing-cdn.css" {
		sheet = wallpaperRules(read("static/landing.css"), base, false)
	} else {
		sheet = wallpaperRules(read("static/bg.css"), base, false) + wallpaperRules(schemeWallpaperCSS, base, true)
	}
	body := minifiedCSS([]byte(sheet))
	cdnSheets.Store(base+path, body)
	return body, true
}

// serveCDNStylesheet answers for a CDN stylesheet. The sheet depends on
// the configured host as well as the build, so it revalidates by ETag.
func serveCDNStylesheet(w http.ResponseWriter, req *http.Request, body []byte) {
	sum := sha256.Sum256(body)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
	http.ServeContent(w, req, req.URL.Path, time.Time{}, bytes.NewReader(body))
}
