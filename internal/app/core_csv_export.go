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

func sanitizeCSVCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '@', '\t', '\r':
		return "'" + s
	case '-':
		if len(s) == 1 || s[1] < '0' || s[1] > '9' {
			return "'" + s
		}
	}
	return s
}

func sanitizeCSVRow(row []string) []string {
	out := make([]string, len(row))
	for i, c := range row {
		out[i] = sanitizeCSVCell(c)
	}
	return out
}

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
	cw.Write(sanitizeCSVRow(header))
	for _, row := range rows {
		cw.Write(sanitizeCSVRow(row))
	}
	cw.Flush()
}
