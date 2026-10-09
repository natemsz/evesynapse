package app

// CSV export for market tables. Handlers check ?format=csv and
// call serveCSV with the same rows they already built for the
// HTML view; no recomputation, just reformatting.

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"time"
)

// serveCSV writes rows as a CSV download. filename is the base
// name without extension; the date is appended for uniqueness.
func serveCSV(w http.ResponseWriter, filename string, header []string, rows [][]string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		`attachment; filename="%s-%s.csv"`,
		filename, time.Now().Format("2006-01-02")))
	// UTF-8 BOM so Excel opens it correctly.
	w.Write([]byte("\xef\xbb\xbf"))
	cw := csv.NewWriter(w)
	cw.Write(header)
	for _, row := range rows {
		cw.Write(row)
	}
	cw.Flush()
}

// wantCSV reports whether the request asks for CSV instead of HTML.
func wantCSV(r *http.Request) bool {
	return r.URL.Query().Get("format") == "csv"
}
