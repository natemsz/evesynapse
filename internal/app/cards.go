package app

// ---------------------------------------------------------------------------
// Summary cards: the PNG a shareable page's og:image points at,
// drawn by internal/card from the page's own stored data.
//
// A card route is a generator, nothing is pre-made per entity. The
// first request for an entity reads the same stored rows its page
// reads (bounded single-row reads, never ESI or zKillboard), draws
// the card and keeps the PNG in a byte-capped LRU. The cache key
// carries the data version (for a pilot, the activity profile's
// updated_at and the record's fetch time; for a kill, its stored
// value and names), so a card redraws when its figures move and is
// served from memory otherwise. Portraits and ship renders come
// from images.evetech.net, fetched once into their own capped cache.
//
// An entity that does not resolve gets the global fallback card
// (the front page's share image), never a blank or broken image.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // portraits and renders arrive as JPEG or PNG
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	json "github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"evesynapse/internal/app/module"
	"evesynapse/internal/card"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	// cardCacheBytes caps the drawn PNGs kept in memory (a card is
	// about 100-250 KB, so a few hundred of the most recently
	// shared entities).
	cardCacheBytes = 64 << 20
	// cardPictureBytes caps the fetched portraits and renders.
	cardPictureBytes = 24 << 20
	// cardDrawSlots bounds concurrent draws: a crawler walking card
	// URLs queues behind them instead of taking the CPU.
	cardDrawSlots = 2
	// cardPictureRetry is how long a failed picture fetch is not
	// retried; the card draws without it meanwhile and is not kept.
	cardPictureRetry = 10 * time.Minute
	// cardPictureMax bounds one fetched picture's size.
	cardPictureMax = 4 << 20
	// cardStyle versions the drawing itself: bump it when the
	// template changes so clients holding an ETag fetch anew.
	cardStyle = "1"
)

// cardState is the generator's caches and limits, made on first use
// (the test rig builds an Application without New).
type cardState struct {
	cards    *byteLRU
	pictures *byteLRU
	slots    chan struct{}
	group    singleflight.Group
	// draws counts cards actually drawn, so a served-from-cache
	// request is observable.
	draws atomic.Int64
}

func (app *Application) cardsState() *cardState {
	app.cardOnce.Do(func() {
		app.cards = &cardState{
			cards:    newByteLRU(cardCacheBytes),
			pictures: newByteLRU(cardPictureBytes),
			slots:    make(chan struct{}, cardDrawSlots),
		}
	})
	return app.cards
}

// cardJob is one card ready to draw: its cache key, its data
// version, its picture's address (and one to try when the image
// server has no such picture, an item's icon for its render) and
// the content.
type cardJob struct {
	key        string
	version    string
	picture    string
	pictureAlt string
	card       card.Card
	maxAge     time.Duration
}

// cardVersion hashes a card's data version with everything drawn on
// it, so any change in what the card shows changes the version.
func cardVersion(data string, picture string, c card.Card) string {
	h := sha256.New()
	// Every field by value: a pointer printed with %+v would hash
	// its address and change the version on every request.
	var detail card.Detail
	if c.Detail != nil {
		detail = *c.Detail
	}
	var badge card.Badge
	if c.Badge != nil {
		badge = *c.Badge
	}
	var spark card.Spark
	if c.Spark != nil {
		spark = *c.Spark
	}
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%q\x00%q\x00%q\x00%+v\x00%+v\x00%+v\x00%+v\x00%+v\x00%t",
		cardStyle, data, picture, c.Kind, c.Title, c.Subtitle, c.Stats, detail, badge, c.Bars, spark, c.Mark)
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// handlePilotCard serves /pilot/card.png?character={id}.
func (app *Application) handlePilotCard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64)
	if err != nil || id <= 0 {
		app.serveFallbackCard(w)
		return
	}
	job, ok := app.pilotCardJob(r.Context(), id)
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	app.serveCard(w, r, job)
}

// handleKillCard serves /intel/kill/{id}/card.png.
func (app *Application) handleKillCard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		app.serveFallbackCard(w)
		return
	}
	job, ok := app.killCardJob(r.Context(), id)
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	app.serveCard(w, r, job)
}

// pilotCardJob reads a pilot's card from their stored record and
// activity profile: the activity panel's lead figures, from the
// same rows. A pilot without a ready record does not resolve; one
// without an activity record gets a name and portrait card.
func (app *Application) pilotCardJob(ctx context.Context, id int64) (cardJob, bool) {
	rec, err := app.queries.GetPilotRecord(ctx, id)
	if err != nil || rec.State != pilotStateReady || rec.Payload == "" {
		return cardJob{}, false
	}
	var p struct {
		Profile struct {
			Name string `json:"name"`
		} `json:"profile"`
		Corp struct {
			Name string `json:"name"`
		} `json:"corp"`
	}
	if json.Unmarshal([]byte(rec.Payload), &p) != nil || p.Profile.Name == "" {
		return cardJob{}, false
	}
	c := card.Card{Kind: "PILOT", Title: p.Profile.Name, Subtitle: p.Corp.Name}
	data := "record:" + rec.FetchedAt.Time.UTC().Format(time.RFC3339Nano)
	prof, err := app.queries.GetPilotActivityProfile(ctx, id)
	switch {
	case err == nil:
		data += "|profile:" + prof.UpdatedAt.UTC().Format(time.RFC3339Nano)
		if prof.Kills+prof.Losses > 0 {
			c.Stats = []card.Stat{
				{Label: "KILLS", Value: card.Figure(esi.FormatInt(prof.Kills))},
				{Label: "LOSSES", Value: card.Figure(esi.FormatInt(prof.Losses))},
			}
			if st, err := app.pilotAllTimeStats(ctx, id); err == nil && st.IskKilled > 0 {
				c.Stats = append(c.Stats, card.Stat{Label: "ISK DESTROYED", Value: card.Figure(iskShortGo(st.IskKilled))})
			}
		}
	case !errors.Is(err, pgx.ErrNoRows):
		logging.Warnf("pilot card %d: profile: %v", id, err)
	}
	pic := portraitURL(id, 512)
	return cardJob{key: "pilot:" + strconv.FormatInt(id, 10), version: cardVersion(data, pic, c),
		picture: pic, card: c, maxAge: time.Hour}, true
}

// killCardJob reads a public kill's card: the ship render, the
// victim and their ship, the stored priced value (never a
// recomputation), the system and the final blow.
func (app *Application) killCardJob(ctx context.Context, id int64) (cardJob, bool) {
	k, err := app.queries.GetPublicKill(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Warnf("kill card %d: %v", id, err)
		}
		return cardJob{}, false
	}
	names := app.newKillNames(ctx)
	row := names.row(k)
	app.kickNameResolve()
	c := card.Card{Kind: "KILL", Title: row.VictimName}
	if row.VictimIsStructure {
		c.Subtitle = row.CorpName
	} else {
		c.Subtitle = joinNonEmpty(" · ", row.ShipName, row.CorpName)
	}
	if row.Value != "" {
		c.Stats = append(c.Stats, card.Stat{Label: "VALUE", Value: card.Figure(row.Value)})
	}
	if row.SystemName != "" {
		c.Stats = append(c.Stats, card.Stat{Label: "SYSTEM", Value: []card.Seg{
			{Text: row.SecText + " ", Color: card.SecColor(row.SecTier)}, {Text: row.SystemName},
		}})
	}
	if fb := joinNonEmpty(" · ", row.FinalName, row.FinalCorpName); fb != "" {
		c.Detail = &card.Detail{Label: "FINAL BLOW", Text: fb}
	}
	maxAge := 10 * time.Minute
	if row.Value != "" {
		// A priced kill never changes.
		maxAge = 24 * time.Hour
	}
	pic := fmt.Sprintf("https://images.evetech.net/types/%d/render?size=512", k.ShipTypeID)
	return cardJob{key: "kill:" + strconv.FormatInt(id, 10), version: cardVersion("", pic, c),
		picture: pic, card: c, maxAge: maxAge}, true
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// serveCard answers from the cache when the stored version matches
// and draws otherwise; concurrent requests for the same version
// share one draw.
func (app *Application) serveCard(w http.ResponseWriter, r *http.Request, job cardJob) {
	etag := `"` + job.version + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(job.maxAge.Seconds())))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	st := app.cardsState()
	if e, ok := st.cards.get(job.key); ok && e.ver == job.version {
		writeCardPNG(w, e.data)
		return
	}
	v, err, _ := st.group.Do(job.key+"@"+job.version, func() (any, error) {
		// The draw outlives any one request: the first caller's
		// cancellation must not fail the others waiting on it.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		select {
		case st.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-st.slots }()
		pic, complete := app.cardPicture(ctx, job.picture)
		if pic == nil && job.pictureAlt != "" {
			pic, complete = app.cardPicture(ctx, job.pictureAlt)
		}
		c := job.card
		c.Picture = pic
		data, err := card.PNG(c)
		if err != nil {
			return nil, err
		}
		st.draws.Add(1)
		if complete {
			st.cards.put(lruEntry{key: job.key, ver: job.version, data: data})
		}
		return data, nil
	})
	if err != nil {
		logging.Warnf("card %s: %v", job.key, err)
		app.serveFallbackCard(w)
		return
	}
	writeCardPNG(w, v.([]byte))
}

func writeCardPNG(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// fallbackCard is the global share image, the front page's.
var fallbackCard = sync.OnceValue(func() []byte {
	data, err := staticFS.ReadFile("static" + strings.TrimPrefix(module.FallbackImagePath, "/static"))
	if err != nil {
		logging.Errorf("card: fallback image: %v", err)
	}
	return data
})

// serveFallbackCard answers with the global share image. It is
// cached briefly: an entity that resolves later gets its own card.
func (app *Application) serveFallbackCard(w http.ResponseWriter) {
	data := fallbackCard()
	w.Header().Del("ETag")
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// cardHTTP fetches card pictures.
var cardHTTP = &http.Client{Timeout: 8 * time.Second}

// cardPicture returns a card's picture from the picture cache,
// fetching it once on a miss. complete is false when the picture
// could not be had: the card then draws without it and is not
// cached, so a later request tries again.
func (app *Application) cardPicture(ctx context.Context, url string) (image.Image, bool) {
	if url == "" {
		return nil, true
	}
	st := app.cardsState()
	e, ok := st.pictures.get(url)
	if !ok || (e.data == nil && time.Now().After(e.until)) {
		fetch := app.cardFetch
		if fetch == nil {
			fetch = fetchCardPicture
		}
		data, err := fetch(ctx, url)
		if err != nil {
			logging.Warnf("card picture %s: %v", url, err)
			st.pictures.put(lruEntry{key: url, until: time.Now().Add(cardPictureRetry)})
			return nil, false
		}
		e = lruEntry{key: url, data: data}
		st.pictures.put(e)
	}
	if e.data == nil {
		return nil, false
	}
	img, _, err := image.Decode(bytes.NewReader(e.data))
	if err != nil {
		logging.Warnf("card picture %s: decode: %v", url, err)
		return nil, false
	}
	return img, true
}

func fetchCardPicture(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "EveSynapse (+https://evesynapse.app)")
	resp, err := cardHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, cardPictureMax+1))
	if err != nil {
		return nil, err
	}
	if len(data) > cardPictureMax {
		return nil, errors.New("picture too large")
	}
	return data, nil
}

// byteLRU is a least-recently-used map of byte payloads capped by
// their total size. One entry per key: a new version replaces the
// old one.
type byteLRU struct {
	mu   sync.Mutex
	max  int
	used int
	ll   *list.List
	m    map[string]*list.Element
}

// lruEntry is one cached payload. A nil data with until set is a
// remembered failure.
type lruEntry struct {
	key   string
	ver   string
	data  []byte
	until time.Time
}

func newByteLRU(max int) *byteLRU {
	return &byteLRU{max: max, ll: list.New(), m: make(map[string]*list.Element)}
}

func (c *byteLRU) get(key string) (lruEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[key]
	if !ok {
		return lruEntry{}, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(lruEntry), true
}

// size is an entry's charge against the cap: its payload plus a
// fixed overhead, so remembered failures are bounded too.
func (e lruEntry) size() int { return len(e.data) + 256 }

func (c *byteLRU) put(e lruEntry) {
	if e.size() > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[e.key]; ok {
		c.used -= el.Value.(lruEntry).size()
		c.ll.Remove(el)
	}
	c.m[e.key] = c.ll.PushFront(e)
	c.used += e.size()
	for c.used > c.max {
		last := c.ll.Back()
		old := last.Value.(lruEntry)
		c.ll.Remove(last)
		delete(c.m, old.key)
		c.used -= old.size()
	}
}
