package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// The importer reads CCP's SDE API. GET <base>latest/ names the
// current release and lists every table with a version that changes
// only when that table's data does. The importer stores the version of
// each table it imported; a check imports when any of them moved, and
// an import downloads only the tables whose version differs from the
// copy kept in sde_api_blobs.

// sdeCheckEvery is how often the worker asks the SDE API for a new
// release. CCP publishes one whenever the data changes, not only at
// downtime, and the release list is a small cached request.
const sdeCheckEvery = 30 * time.Minute

// sdeAPIMaxTableBytes bounds one downloaded table as served
// (gzipped); the largest, mapMoons, is under 40 MB.
const sdeAPIMaxTableBytes = 256 << 20

// sdeRelease is the SDE API's description of one release.
type sdeRelease struct {
	Release     string                     `json:"release"`
	BuildNumber int64                      `json:"buildNumber"`
	ReleaseDate string                     `json:"releaseDate"`
	Tables      map[string]sdeReleaseTable `json:"tables"`
}

type sdeReleaseTable struct {
	Version string `json:"version"`
	JSONL   struct {
		URL string `json:"url"`
	} `json:"jsonl"`
}

// label names the release for logs and the Sync page.
func (r *sdeRelease) label() string {
	return fmt.Sprintf("release %s (build %d)", r.Release, r.BuildNumber)
}

// fetchSDERelease reads the current release from the SDE API.
func (app *Application) fetchSDERelease(ctx context.Context) (*sdeRelease, error) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, app.cfg.SDEAPIURL()+"latest/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", sdeUserAgent)
	resp, err := sdeHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SDE release list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SDE release list: status %d", resp.StatusCode)
	}
	var rel sdeRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("SDE release list: %w", err)
	}
	if rel.Release == "" || len(rel.Tables) == 0 {
		return nil, errors.New("SDE release list: no release or no tables")
	}
	return &rel, nil
}

// sdeAPIChangedTables lists the tables the importer reads whose
// version in rel differs from the one stored at the last import.
// Every table counts as changed before the first import from the API;
// a table the release does not list never does, since importing again
// would not bring it.
func (app *Application) sdeAPIChangedTables(ctx context.Context, rel *sdeRelease) ([]string, error) {
	stored, err := app.storedSDEVersions(ctx)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, t := range sdeAPITables {
		if v := rel.Tables[t.name].Version; v != "" && v != stored[t.name] {
			changed = append(changed, t.name)
		}
	}
	return changed, nil
}

// storedSDEVersions reads the table versions of the last import.
func (app *Application) storedSDEVersions(ctx context.Context) (map[string]string, error) {
	rows, err := app.queries.ListSDEMetaPrefix(ctx, "version:")
	if err != nil {
		return nil, fmt.Errorf("read stored SDE versions: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[strings.TrimPrefix(r.Key, "version:")] = r.Value
	}
	return out, nil
}

// sdeAPITableBody returns one table of rel as the API serves it: the
// kept copy when its version matches, else a fresh download, which is
// kept for the next release. fresh says whether it was downloaded.
func (app *Application) sdeAPITableBody(ctx context.Context, rel *sdeRelease, table string) (body []byte, fresh bool, err error) {
	t, ok := rel.Tables[table]
	if !ok || t.JSONL.URL == "" || t.Version == "" {
		return nil, false, fmt.Errorf("%s is not in %s", table, rel.label())
	}
	if row, err := app.queries.GetSDEAPIBlob(ctx, table); err == nil && row.Version == t.Version {
		return row.Body, false, nil
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		logging.Warnf("sde: read kept copy of %s: %v", table, err)
	}
	body, err = app.downloadSDETable(ctx, t)
	if err != nil {
		return nil, false, fmt.Errorf("download %s: %w", table, err)
	}
	return body, true, nil
}

// keepSDETable stores a downloaded table once it parsed cleanly.
func (app *Application) keepSDETable(ctx context.Context, table, version string, body []byte) {
	if err := app.queries.PutSDEAPIBlob(ctx, db.PutSDEAPIBlobParams{
		TableName: table, Version: version, Body: body, FetchedAt: time.Now().UTC(),
	}); err != nil {
		logging.Warnf("sde: keep copy of %s: %v", table, err)
	}
}

// downloadSDETable fetches one table gzipped, as the API stores it.
// The served ETag must name the version the release lists, so a body
// from another release is never kept under this one's version.
func (app *Application) downloadSDETable(ctx context.Context, t sdeReleaseTable) ([]byte, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, t.JSONL.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", sdeUserAgent)
	// Asking for gzip by hand keeps the transport from unpacking it,
	// so the kept copy stays small.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := sdeHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	if etag := strings.Trim(strings.TrimPrefix(resp.Header.Get("ETag"), "W/"), `"`); etag != "" && etag != t.Version {
		return nil, fmt.Errorf("served version %s, expected %s", etag, t.Version)
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(resp.Body, sdeAPIMaxTableBytes+1))
	if err != nil {
		return nil, err
	}
	if n > sdeAPIMaxTableBytes {
		return nil, fmt.Errorf("larger than %d MB", sdeAPIMaxTableBytes>>20)
	}
	return buf.Bytes(), nil
}
