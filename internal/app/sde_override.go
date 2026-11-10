package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// An uploaded dump stands in for the regular one when the game has
// changed and the regular dump has not caught up yet. It is a zip of
// dump CSV files (plain or .bz2, any folder layout) under the same
// file names the regular dump uses; files the zip lacks still come
// from the regular dump. The importer drops the upload when an import
// from it fails, and when the regular dump changes after the upload.
const (
	// sdeOverrideMaxBytes bounds the uploaded zip itself.
	sdeOverrideMaxBytes = 96 << 20
	// sdeOverrideMaxFileBytes bounds one unpacked file, the same cap
	// the parser applies to a downloaded one.
	sdeOverrideMaxFileBytes = 512 << 20
)

// sdeKnownFiles is every dump file the importer reads.
func sdeKnownFiles() map[string]bool {
	known := make(map[string]bool, len(sdeFileNames)+7)
	for _, name := range sdeFileNames {
		known[name] = true
	}
	for _, name := range []string{
		sdeSkillsFileName, sdeRangesFileName, sdeMappingsFileName,
		sdeConstellationsFileName, sdeJumpsFileName, sdeCelestialsFileName,
	} {
		known[name] = true
	}
	return known
}

// sdeArchive is an opened upload: its dump files by name.
type sdeArchive struct {
	label string
	sha   string
	files map[string]*zip.File
}

// openSDEArchive reads a zip upload and indexes the dump files in
// it. Entries the importer does not read are ignored; a zip holding
// none of its files, the same file twice, or a file past the size
// cap is refused.
func openSDEArchive(data []byte) (*sdeArchive, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a zip file: %w", err)
	}
	known := sdeKnownFiles()
	files := make(map[string]*zip.File)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := strings.TrimSuffix(path.Base(f.Name), ".bz2")
		if !known[name] {
			continue
		}
		if _, dup := files[name]; dup {
			return nil, fmt.Errorf("%s appears twice", name)
		}
		if f.UncompressedSize64 > sdeOverrideMaxFileBytes {
			return nil, fmt.Errorf("%s unpacks to more than %d MB", name, sdeOverrideMaxFileBytes>>20)
		}
		files[name] = f
	}
	if len(files) == 0 {
		return nil, errors.New("the zip holds none of the dump files the importer reads")
	}
	sum := sha256.Sum256(data)
	return &sdeArchive{sha: hex.EncodeToString(sum[:]), files: files}, nil
}

// names lists the archive's dump files, sorted.
func (a *sdeArchive) names() []string {
	out := make([]string, 0, len(a.files))
	for name := range a.files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// parse feeds one archive file to the dump parser. found is false
// when the archive lacks the file.
func (a *sdeArchive) parse(name string, parsed *parsedSDE) (found bool, err error) {
	f, ok := a.files[name]
	if !ok {
		return false, nil
	}
	rc, err := f.Open()
	if err != nil {
		return true, fmt.Errorf("open %s in upload: %w", name, err)
	}
	defer rc.Close()
	return true, parseSDEFile(name, io.LimitReader(rc, sdeOverrideMaxFileBytes), parsed)
}

// source names the upload for the importer's source line.
func (a *sdeArchive) source(base string) string {
	return fmt.Sprintf("uploaded file %q (sha256 %s), other files from %s", a.label, a.sha[:12], base)
}

// loadSDEOverride returns the active upload, or nil when there is
// none. An upload that no longer opens is dropped and reported.
func (app *Application) loadSDEOverride(ctx context.Context) *sdeArchive {
	row, err := app.queries.GetSDEOverride(ctx)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("sde: load uploaded dump: %v", err)
		}
		return nil
	}
	arc, err := openSDEArchive(row.Archive)
	if err != nil {
		logging.Errorf("sde: uploaded dump %q no longer opens, dropping it: %v", row.Label, err)
		app.dropSDEOverride(ctx, row.Sha256)
		return nil
	}
	arc.label = row.Label
	return arc
}

// dropSDEOverride removes the upload with this hash (a newer upload
// that replaced it in the meantime stays).
func (app *Application) dropSDEOverride(ctx context.Context, sha string) {
	if err := app.queries.DeleteSDEOverrideIfSHA(ctx, sha); err != nil {
		logging.Errorf("sde: drop uploaded dump: %v", err)
	}
}

// sdeOverrideView is the Sync page's line about an active upload.
type sdeOverrideView struct {
	Label      string
	SHA        string
	Size       string
	Files      string
	UploadedAt string
}

func (app *Application) loadSDEOverrideView(ctx context.Context) *sdeOverrideView {
	row, err := app.queries.GetSDEOverrideInfo(ctx)
	if err != nil {
		return nil
	}
	return &sdeOverrideView{
		Label:      row.Label,
		SHA:        row.Sha256[:12],
		Size:       fmt.Sprintf("%.1f MB", float64(row.SizeBytes)/(1<<20)),
		Files:      strings.ReplaceAll(row.Files, ",", ", "),
		UploadedAt: row.UploadedAt.UTC().Format(time.RFC3339),
	}
}

// handleSDEOverrideUpload stores an uploaded dump and imports from
// it (POST /admin/sync/sde/upload, multipart: label, archive).
func (app *Application) handleSDEOverrideUpload(w http.ResponseWriter, r *http.Request) {
	back := "/admin/sync/#sync-sde"
	fail := func(msg string) {
		app.updateSDEStatus(func(s *sdeStatus) { s.LastError = "Upload refused: " + msg })
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
	r.Body = http.MaxBytesReader(w, r.Body, sdeOverrideMaxBytes+64<<10)
	mr, err := r.MultipartReader()
	if err != nil {
		fail("expected a file upload form")
		return
	}
	var label string
	var data []byte
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(fmt.Sprintf("the upload is larger than %d MB or broken", sdeOverrideMaxBytes>>20))
			return
		}
		switch part.FormName() {
		case "label":
			b, _ := io.ReadAll(io.LimitReader(part, 200))
			label = strings.TrimSpace(string(b))
		case "archive":
			data, err = io.ReadAll(part)
			if err != nil {
				fail(fmt.Sprintf("the upload is larger than %d MB or broken", sdeOverrideMaxBytes>>20))
				return
			}
		}
		part.Close()
	}
	if len(data) == 0 {
		fail("no file chosen")
		return
	}
	arc, err := openSDEArchive(data)
	if err != nil {
		fail(err.Error())
		return
	}
	if label == "" {
		label = "upload " + time.Now().UTC().Format("2006-01-02 15:04")
	}
	ctx := r.Context()
	if err := app.queries.PutSDEOverride(ctx, db.PutSDEOverrideParams{
		Label:      label,
		Archive:    data,
		Sha256:     arc.sha,
		SizeBytes:  int64(len(data)),
		Files:      strings.Join(arc.names(), ","),
		UploadedBy: app.userID(ctx),
	}); err != nil {
		logging.Errorf("sde: store uploaded dump: %v", err)
		fail("could not store the file")
		return
	}
	logging.Infof("sde: dump %q uploaded by user %d (%d files, sha256 %s)", label, app.userID(ctx), len(arc.files), arc.sha[:12])
	if !app.startSDEImport("uploaded dump") {
		app.updateSDEStatus(func(s *sdeStatus) {
			s.Message = "Upload stored. Another import is running; the next one reads the upload."
		})
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// handleSDEOverrideClear drops the upload and imports the regular
// dump again (POST /admin/sync/sde/upload/clear).
func (app *Application) handleSDEOverrideClear(w http.ResponseWriter, r *http.Request) {
	if err := app.queries.DeleteSDEOverride(r.Context()); err != nil {
		logging.Errorf("sde: clear uploaded dump: %v", err)
	} else {
		logging.Infof("sde: uploaded dump removed by user %d", app.userID(r.Context()))
		if !app.startSDEImport("upload removed") {
			app.updateSDEStatus(func(s *sdeStatus) {
				s.Message = "Upload removed. Another import is running; run a check afterwards to load the regular dump."
			})
		}
	}
	http.Redirect(w, r, "/admin/sync/#sync-sde", http.StatusSeeOther)
}
