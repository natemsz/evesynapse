package app

// The live feeds: Server-Sent Events streams that push new kills and
// market stat-pack updates to the browser as they arrive, so the
// pages stay current without a reload. The hubs are in-process and
// bounded: a slow or dead browser never blocks ingestion or the
// worker, because every publish is a non-blocking send and a full
// subscriber channel drops the event instead of stalling the
// publisher. The killboard and the market item page keep their
// server-rendered first paint; the streams only add rows and values
// after load.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// maxLiveFeedSubscribers caps concurrent SSE connections per hub.
// Past the cap a new stream is refused with 503, never queued.
const maxLiveFeedSubscribers = 200

// liveFeedChanCap bounds how many unpublished events one subscriber
// may lag behind. A subscriber that falls further behind drops
// events; its page still renders, it just misses a live row.
const liveFeedChanCap = 32

// liveFeedHeartbeat is how often the stream writes a comment line
// so an idle connection stays open through proxies and a dead one
// is noticed by the failed write.
const liveFeedHeartbeat = 30 * time.Second

// liveFeedRetry tells the browser how long to wait before it
// reconnects after a dropped stream.
const liveFeedRetry = 5 * time.Second

// killLiveSub is one kill-stream subscriber: its channel and the
// scope it asked for. An empty kind streams every public kill.
type killLiveSub struct {
	ch   chan []byte
	kind string // "", "system", "region", "ship", "character", "corporation", "alliance"
	id   int64
	big  bool
	solo bool
}

// killLiveHub is the kill feed's fan-out: one publish per new kill,
// fanned out to the subscribers whose scope matches, never blocking.
type killLiveHub struct {
	mu   sync.Mutex
	subs map[*killLiveSub]struct{}
}

func (h *killLiveHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *killLiveHub) subscribe(kind string, id int64, big, solo bool) (*killLiveSub, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= maxLiveFeedSubscribers {
		return nil, false
	}
	if h.subs == nil {
		h.subs = make(map[*killLiveSub]struct{})
	}
	sub := &killLiveSub{ch: make(chan []byte, liveFeedChanCap), kind: kind, id: id, big: big, solo: solo}
	h.subs[sub] = struct{}{}
	return sub, true
}

func (h *killLiveHub) unsubscribe(sub *killLiveSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, sub)
}

// killMatchesScope reports whether kill k belongs on a stream scoped
// to kind/id with the big and solo narrowings. The value test reads
// the kill's own appraisal when present; an unpriced kill never
// matches a big-only stream.
func killMatchesScope(k db.Kill, kind string, id int64, big, solo bool) bool {
	if solo && !k.Solo {
		return false
	}
	if big && (!k.ValueOurs.Valid || k.ValueOurs.Float64 < killBigValue) {
		return false
	}
	switch kind {
	case "":
		return true
	case "system":
		return k.SystemID == id
	case "region":
		return k.RegionID == id
	case "ship":
		return k.ShipTypeID == id
	case "character":
		return k.VictimCharacterID == id || k.FbCharacterID == id
	case "corporation":
		return k.VictimCorporationID == id || k.FbCorporationID == id
	case "alliance":
		return k.VictimAllianceID == id || k.FbAllianceID == id
	}
	return false
}

// killPriceLiveEvent is the payload of a kill-price event: the value
// that landed on a kill already streamed, so the row's value cell
// fills in without a reload.
type killPriceLiveEvent struct {
	ID    int64  `json:"id"`
	Value string `json:"value"` // short ISK, "" while unpriced
	Big   bool   `json:"big"`
}

// sseFrame renders one complete SSE event: the event line, the data
// line, and the blank line that ends the event.
func sseFrame(event string, payload []byte) []byte {
	frame := make([]byte, 0, len(event)+len(payload)+16)
	frame = append(frame, []byte("event:"+event+"\n")...)
	frame = append(frame, []byte("data:")...)
	frame = append(frame, payload...)
	frame = append(frame, []byte("\n\n")...)
	return frame
}

// publishKill fans one new kill out to the subscribers whose scope
// matches and who do not narrow to big kills. A big-only stream
// learns about a kill at price time, when its value is known
// (publishKillPriced): at insert time the kill is still unpriced,
// so no big determination exists yet. The row is serialized once;
// a subscriber whose channel is full drops the event instead of
// blocking the publisher.
func (h *killLiveHub) publishKill(row killRow, k db.Kill) {
	payload, err := json.Marshal(row)
	if err != nil {
		logging.Errorf("live feed: marshal kill %d: %v", row.ID, err)
		return
	}
	h.mu.Lock()
	subs := make([]*killLiveSub, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	frame := sseFrame("kill", payload)
	for _, sub := range subs {
		if sub.big {
			continue
		}
		if !killMatchesScope(k, sub.kind, sub.id, false, sub.solo) {
			continue
		}
		select {
		case sub.ch <- frame:
		default:
		}
	}
}

// publishKillPriced runs when a kill's appraisal lands. Subscribers
// that already saw the kill get its value; big-only subscribers get
// the whole kill, but only when the appraisal makes it big. A
// subscriber that never saw the kill (it connected after the kill
// streamed) ignores the price event: its page never rendered that
// row.
func (h *killLiveHub) publishKillPriced(row killRow, k db.Kill, value string, big bool) {
	pricePayload, err := json.Marshal(killPriceLiveEvent{ID: k.KillmailID, Value: value, Big: big})
	if err != nil {
		return
	}
	h.mu.Lock()
	subs := make([]*killLiveSub, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	priceFrame := sseFrame("kill-price", pricePayload)
	var killFrame []byte
	if big {
		killPayload, err := json.Marshal(row)
		if err != nil {
			logging.Errorf("live feed: marshal priced kill %d: %v", row.ID, err)
		} else {
			killFrame = sseFrame("kill", killPayload)
		}
	}
	for _, sub := range subs {
		if sub.big {
			if big && killFrame != nil && killMatchesScope(k, sub.kind, sub.id, true, sub.solo) {
				select {
				case sub.ch <- killFrame:
				default:
				}
			}
			continue
		}
		if !killMatchesScope(k, sub.kind, sub.id, false, sub.solo) {
			continue
		}
		select {
		case sub.ch <- priceFrame:
		default:
		}
	}
}

// marketLiveKey is one market stream's subscription: one type in one
// region.
type marketLiveKey struct {
	regionID int64
	typeID   int64
}

// marketLiveSub is one market-stream subscriber.
type marketLiveSub struct {
	ch  chan []byte
	key marketLiveKey
}

// marketLiveHub is the stat-pack fan-out, indexed by (region, type)
// so a region refresh only wakes the subscribers watching one of its
// types.
type marketLiveHub struct {
	mu    sync.Mutex
	byKey map[marketLiveKey]map[*marketLiveSub]struct{}
}

func (h *marketLiveHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, subs := range h.byKey {
		n += len(subs)
	}
	return n
}

func (h *marketLiveHub) subscribe(regionID, typeID int64) (*marketLiveSub, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.countLocked() >= maxLiveFeedSubscribers {
		return nil, false
	}
	if h.byKey == nil {
		h.byKey = make(map[marketLiveKey]map[*marketLiveSub]struct{})
	}
	key := marketLiveKey{regionID: regionID, typeID: typeID}
	sub := &marketLiveSub{ch: make(chan []byte, liveFeedChanCap), key: key}
	if h.byKey[key] == nil {
		h.byKey[key] = make(map[*marketLiveSub]struct{})
	}
	h.byKey[key][sub] = struct{}{}
	return sub, true
}

func (h *marketLiveHub) countLocked() int {
	n := 0
	for _, subs := range h.byKey {
		n += len(subs)
	}
	return n
}

func (h *marketLiveHub) unsubscribe(sub *marketLiveSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if subs, ok := h.byKey[sub.key]; ok {
		delete(subs, sub)
		if len(subs) == 0 {
			delete(h.byKey, sub.key)
		}
	}
}

// hasSubscriber reports whether any stream is watching this
// (region, type), so a region refresh only formats events someone
// is waiting for.
func (h *marketLiveHub) hasSubscriber(regionID, typeID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.byKey[marketLiveKey{regionID: regionID, typeID: typeID}]) > 0
}

// publish sends one stat-pack update to the subscribers watching
// this (region, type). A full channel drops the event instead of
// blocking the refresh.
func (h *marketLiveHub) publish(regionID, typeID int64, payload []byte) {
	h.mu.Lock()
	subs := make([]*marketLiveSub, 0, len(h.byKey[marketLiveKey{regionID: regionID, typeID: typeID}]))
	for sub := range h.byKey[marketLiveKey{regionID: regionID, typeID: typeID}] {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	for _, sub := range subs {
		select {
		case sub.ch <- payload:
		default:
		}
	}
}

// marketLiveSide is one side of a stat-pack update: the display
// strings the item page's statistics table shows, formatted the
// same way the server renders them.
type marketLiveSide struct {
	Typical string `json:"typical"`
	Average string `json:"average"`
	Best    string `json:"best"`
	Far     string `json:"far"`
	Edge    string `json:"edge"`
	Dev     string `json:"dev"`
	Orders  string `json:"orders"`
	Volume  string `json:"volume"`
}

// marketLiveEvent is the payload of a market stat-pack update: the
// refreshed row for one type in one region.
type marketLiveEvent struct {
	TypeID   int64          `json:"type"`
	RegionID int64          `json:"region"`
	Sell     marketLiveSide `json:"sell"`
	Buy      marketLiveSide `json:"buy"`
	Age      string         `json:"age"`
}

// marketLiveEventFor formats one refreshed stat row as a market
// event. The strings match the item page's statistics table and
// region strip, so the browser can write them into the cells
// without formatting anything itself.
func marketLiveEventFor(row db.MarketRegionStat, now time.Time) marketLiveEvent {
	return marketLiveEvent{
		TypeID:   row.TypeID,
		RegionID: row.RegionID,
		Sell: marketLiveSide{
			Typical: esi.FormatISK(row.TypicalSell), Average: esi.FormatISK(row.SellAvg),
			Best: esi.FormatISK(row.BestSell), Far: esi.FormatISK(row.SellMax),
			Edge: esi.FormatISK(row.SellP5), Dev: esi.FormatISK(row.SellStddev),
			Orders: esi.FormatInt(row.SellOrders), Volume: esi.FormatInt(row.SellVolume),
		},
		Buy: marketLiveSide{
			Typical: esi.FormatISK(row.TypicalBuy), Average: esi.FormatISK(row.BuyAvg),
			Best: esi.FormatISK(row.BestBuy), Far: esi.FormatISK(row.BuyMin),
			Edge: esi.FormatISK(row.BuyP5), Dev: esi.FormatISK(row.BuyStddev),
			Orders: esi.FormatInt(row.BuyOrders), Volume: esi.FormatInt(row.BuyVolume),
		},
		Age: ageText(now, row.UpdatedAt),
	}
}

// serveSSE writes a stream of pre-framed events to w until the
// client disconnects or a write fails. Each event is already a
// complete SSE frame (an "event:" line plus its "data:" lines and
// the blank line); the loop only frames the heartbeat comments.
// Compression is off for the stream: a compressor would buffer a
// trickle that must arrive as it is written.
func serveSSE(w http.ResponseWriter, r *http.Request, ch <-chan []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming is not supported here.", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set("No-Gzip-Compression", "true")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: %d\n\n", liveFeedRetry.Milliseconds())
	flusher.Flush()

	ctx := r.Context()
	beat := time.NewTicker(liveFeedHeartbeat)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case frame, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// killStreamScope reads the killboard's scope and narrowings from a
// stream request's query, the same way handleKillboard reads them
// from the page's.
func killStreamScope(r *http.Request) (kind string, id int64, big, solo bool) {
	qs := r.URL.Query()
	for _, s := range killScopes {
		if n, err := strconv.ParseInt(qs.Get(s), 10, 64); err == nil && n > 0 {
			kind, id = s, n
			break
		}
	}
	return kind, id, qs.Get("big") == "1", qs.Get("solo") == "1"
}

// handleKillStream is GET /intel/kills/stream: the killboard's live
// feed. It is read-only and public like the page, and the global bot
// guard applies to it as to every route. In low-RAM mode the ring
// is off, so there is nothing to stream and the endpoint says so
// with 503 instead of holding an empty connection open.
func (app *Application) handleKillStream(w http.ResponseWriter, r *http.Request) {
	if app.cfg.lowRAMMode {
		http.Error(w, "Live kills are off while the server runs in low-memory mode.", http.StatusServiceUnavailable)
		return
	}
	kind, id, big, solo := killStreamScope(r)
	sub, ok := app.killLive.subscribe(kind, id, big, solo)
	if !ok {
		http.Error(w, "Too many live feeds are open right now. Try again shortly.", http.StatusServiceUnavailable)
		return
	}
	defer app.killLive.unsubscribe(sub)
	serveSSE(w, r, sub.ch)
}

// handleMarketStream is GET /market/stream: one type's stat-pack
// updates in one region, for the market item page. The stream only
// carries rows a region refresh just wrote; the page's first paint
// is still the server render. In low-RAM mode the stat-pack
// snapshot is off, so there is nothing to stream.
func (app *Application) handleMarketStream(w http.ResponseWriter, r *http.Request) {
	if app.cfg.lowRAMMode {
		http.Error(w, "Live prices are off while the server runs in low-memory mode.", http.StatusServiceUnavailable)
		return
	}
	qs := r.URL.Query()
	regionID, err := strconv.ParseInt(qs.Get("region"), 10, 64)
	if err != nil || regionID <= 0 {
		http.Error(w, "A region is required.", http.StatusBadRequest)
		return
	}
	typeID, err := strconv.ParseInt(qs.Get("type"), 10, 64)
	if err != nil || typeID <= 0 {
		http.Error(w, "A type is required.", http.StatusBadRequest)
		return
	}
	sub, ok := app.marketLive.subscribe(regionID, typeID)
	if !ok {
		http.Error(w, "Too many live feeds are open right now. Try again shortly.", http.StatusServiceUnavailable)
		return
	}
	defer app.marketLive.unsubscribe(sub)
	serveSSE(w, r, sub.ch)
}

// publishMarketStatRow fans one refreshed stat row out to the streams
// watching its (region, type). It formats the event only when a
// subscriber waits for it, so a region refresh with no viewers
// costs the refresh nothing.
func (app *Application) publishMarketStatRow(row db.MarketRegionStat) {
	if !app.marketLive.hasSubscriber(row.RegionID, row.TypeID) {
		return
	}
	ev := marketLiveEventFor(row, time.Now())
	payload, err := json.Marshal(ev)
	if err != nil {
		logging.Errorf("live feed: marshal market event for type %d: %v", row.TypeID, err)
		return
	}
	frame := sseFrame("market", payload)
	app.marketLive.publish(row.RegionID, row.TypeID, frame)
}
