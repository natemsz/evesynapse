package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"evesynapse/internal/app/module"
	"evesynapse/internal/logging"
)

// The anonymous landing page: a standalone shell (its own slim
// header, no sidebar) rendered from templates/landing.html. It
// is the site's public face: a hero with the three doors (sign
// up, the demo, the public tools), the tool inventory, what
// sets EveSynapse apart, and the trust points. Everything on it
// is static copy; the page reads no live data at render.

type landingData struct {
	module.ShellData
	Error         string
	SSOConfigured bool
}

func formatISKShort(v float64) string {
	switch {
	case v >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", v/1_000_000_000)
	case v >= 1_000_000:
		return fmt.Sprintf("%.2fM", v/1_000_000)
	case v >= 10_000:
		return fmt.Sprintf("%.1fK", v/1_000)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

// buildLanding assembles the landing page's data.
func (app *Application) buildLanding(ctx context.Context) landingData {
	return landingData{SSOConfigured: app.cfg.SSOConfigured()}
}

func (app *Application) renderLanding(ctx context.Context, w http.ResponseWriter, data landingData) {
	app.fillShell(ctx, &data.ShellData, "landing.html")
	ts, err := parsedTemplate(&pageTemplates, "landing", "landing.html", "templates/wordmark.html", "templates/noticebanners.html")
	if err != nil {
		logging.Errorf("parse template landing.html: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	buf := new(bytes.Buffer)
	if err := ts.ExecuteTemplate(buf, "landing", data); err != nil {
		logging.Errorf("execute template landing.html: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
}
