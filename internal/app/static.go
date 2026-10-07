package app

// ---------------------------------------------------------------------------
// Embedded static assets and how browsers are told to cache them.
//
// The files are embedded in the binary, which gives them no
// modification time, so the file server sends no Last-Modified —
// and with no caching header either, a browser had nothing to go
// on: the stylesheet, the scripts and the wallpaper were fetched in
// full on every page view. Now:
//
//   - Pages link the stylesheet and scripts as ?v=<asset version>,
//     a hash of everything under static/. That URL names exactly
//     those bytes for good, so it is cached for a year; a new build
//     with different files produces different URLs.
//   - Fonts never change at a given URL and cache the same way (as
//     before).
//   - Anything else (the wallpaper the stylesheet references, the
//     favicon, an old ?v= left in an open tab) carries an ETag and
//     is revalidated: one small "not modified" round trip instead
//     of the whole file.
// ---------------------------------------------------------------------------

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
)

const cacheForever = "public, max-age=31536000, immutable"

// staticIndex is the embedded static tree, hashed once.
type staticIndex struct {
	version string            // changes whenever any file under static/ does
	etags   map[string]string // "/style.css" -> `"<content hash>"`
}

var loadStaticIndex = sync.OnceValue(func() staticIndex {
	idx := staticIndex{etags: map[string]string{}}
	var paths []string
	hashes := map[string][]byte{}
	_ = fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		content, err := fs.ReadFile(staticFS, path)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(content)
		name := strings.TrimPrefix(path, "static")
		paths = append(paths, name)
		hashes[name] = sum[:]
		idx.etags[name] = `"` + hex.EncodeToString(sum[:8]) + `"`
		return nil
	})
	sort.Strings(paths)
	all := sha256.New()
	for _, name := range paths {
		all.Write([]byte(name))
		all.Write([]byte{0})
		all.Write(hashes[name])
	}
	idx.version = hex.EncodeToString(all.Sum(nil)[:6])
	return idx
})

// staticAssetVersion is the cache-busting token pages put on their
// stylesheet and script URLs.
func staticAssetVersion() string { return loadStaticIndex().version }

// staticHandler serves the embedded assets under /static/ with the
// caching rules above.
func staticHandler() (http.Handler, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.StripPrefix("/static", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		idx := loadStaticIndex()
		switch {
		case strings.HasPrefix(req.URL.Path, "/fonts/"), req.URL.Query().Get("v") == idx.version:
			w.Header().Set("Cache-Control", cacheForever)
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}
		if etag, ok := idx.etags[req.URL.Path]; ok {
			w.Header().Set("ETag", etag)
		}
		fileServer.ServeHTTP(w, req)
	})), nil
}
