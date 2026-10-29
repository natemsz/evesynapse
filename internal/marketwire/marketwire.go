// Package marketwire is the market API's wire format, version 1: the
// JSON shapes /api/market/v1 answers with. They live apart from the
// app so the format is one versioned contract, changed deliberately,
// and so anything else that moves the same figures (a client, or a
// future instance-to-instance kind) can share the types rather than
// re-describe them.
//
// Every price carries its provenance: the tier that spoke (our own
// swept order book, our stored estimate, the labeled outside
// fallback, a contract-corpus median, an abyssal estimate) and when
// that figure was current. A missing price is absent, never zero.
package marketwire

import "time"

// Version is the format version this package describes.
const Version = 1

// Envelope wraps every successful answer.
type Envelope struct {
	Data any  `json:"data"`
	Meta Meta `json:"meta"`
}

// Meta says where and when an answer came from.
type Meta struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Notes       []string  `json:"notes,omitempty"`
}

// Error is the body of every refusal.
type Error struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody names a refusal: a stable code and a sentence.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Price is one resolved price and its provenance.
type Price struct {
	Value float64   `json:"value"`
	Tier  string    `json:"tier"`
	AsOf  time.Time `json:"as_of"`
	// SourceRegionID is set when a different region's book spoke
	// (a global-market type priced at a hub).
	SourceRegionID int64 `json:"source_region_id,omitempty"`
}

// BookSide is one side of a swept order book: the full statistics
// set, every figure per order row.
type BookSide struct {
	Typical float64 `json:"typical"`
	Average float64 `json:"average"`
	Best    float64 `json:"best"`
	Far     float64 `json:"far"`
	Edge5   float64 `json:"edge_5pct"`
	StdDev  float64 `json:"stddev"`
	Orders  int64   `json:"orders"`
	Units   int64   `json:"units"`
}

// Trend is the stored trend figures of a swept row; nil members had
// no comparison day.
type Trend struct {
	SellChange1d    *float64 `json:"sell_change_1d_pct,omitempty"`
	SellChange7d    *float64 `json:"sell_change_7d_pct,omitempty"`
	SellChange30d   *float64 `json:"sell_change_30d_pct,omitempty"`
	BuyChange1d     *float64 `json:"buy_change_1d_pct,omitempty"`
	BuyChange7d     *float64 `json:"buy_change_7d_pct,omitempty"`
	BuyChange30d    *float64 `json:"buy_change_30d_pct,omitempty"`
	Volatility30d   *float64 `json:"volatility_30d_pct,omitempty"`
	HistoryDays     int64    `json:"history_days"`
	TradedPerDay30d float64  `json:"traded_per_day_30d"`
}

// TypePrice is one type's prices in one region.
type TypePrice struct {
	TypeID   int64  `json:"type_id"`
	RegionID int64  `json:"region_id"`
	Sell     *Price `json:"sell,omitempty"`
	Buy      *Price `json:"buy,omitempty"`
	// The swept book behind the price, when our sweep covers the
	// region; absent when the price came from elsewhere.
	BookSell *BookSide `json:"book_sell,omitempty"`
	BookBuy  *BookSide `json:"book_buy,omitempty"`
	Trend    *Trend    `json:"trend,omitempty"`
	BookAsOf time.Time `json:"book_as_of,omitzero"`
}

// HistoryDay is one stored day of a swept row.
type HistoryDay struct {
	Day         string  `json:"day"`
	TypicalSell float64 `json:"typical_sell"`
	TypicalBuy  float64 `json:"typical_buy"`
	BestSell    float64 `json:"best_sell"`
	BestBuy     float64 `json:"best_buy"`
	SellOrders  int64   `json:"sell_orders"`
	BuyOrders   int64   `json:"buy_orders"`
	SellUnits   int64   `json:"sell_units"`
	BuyUnits    int64   `json:"buy_units"`
}

// Change is one change feed row.
type Change struct {
	TypeID     int64     `json:"type_id"`
	Day        string    `json:"day"`
	Kind       string    `json:"kind"`
	Before     float64   `json:"before"`
	After      float64   `json:"after"`
	Pct        float64   `json:"pct"`
	DetectedAt time.Time `json:"detected_at"`
}

// AppraiseRequest is the body of POST /appraise.
type AppraiseRequest struct {
	RegionID int64          `json:"region_id,omitempty"`
	Mode     string         `json:"mode,omitempty"` // sell (default), buy, split
	Lines    []AppraiseLine `json:"lines"`
}

// AppraiseLine is one line to price: a type and quantity, and the
// facts that make an item unlike its type: a mutated item's id, a
// blueprint's copy flag, runs and research levels.
type AppraiseLine struct {
	TypeID   int64 `json:"type_id"`
	Quantity int64 `json:"quantity"`
	ItemID   int64 `json:"item_id,omitempty"`
	// Blueprint facts; omit for anything else.
	Copy bool  `json:"copy,omitempty"`
	Runs int64 `json:"runs,omitempty"`
	ME   int64 `json:"me,omitempty"`
	TE   int64 `json:"te,omitempty"`
}

// AppraisedLine is one priced (or unpriced) line.
type AppraisedLine struct {
	TypeID    int64     `json:"type_id"`
	Quantity  int64     `json:"quantity"`
	ItemID    int64     `json:"item_id,omitempty"`
	UnitPrice float64   `json:"unit_price"`
	LineValue float64   `json:"line_value"`
	Priced    bool      `json:"priced"`
	Tier      string    `json:"tier"`
	AsOf      time.Time `json:"as_of,omitzero"`
	// Pending: a mutated item queued for its rolls to be read; ask
	// again in a few minutes.
	Pending bool `json:"pending,omitempty"`
	// ResearchUnmatched: a researched original priced at the
	// unresearched original's figure, no research median covering it.
	ResearchUnmatched bool  `json:"research_unmatched,omitempty"`
	SourceRegionID    int64 `json:"source_region_id,omitempty"`
}

// Appraisal is the answer to POST /appraise.
type Appraisal struct {
	RegionID    int64           `json:"region_id"`
	Mode        string          `json:"mode"`
	Total       float64         `json:"total"`
	PricedLines int             `json:"priced_lines"`
	Lines       []AppraisedLine `json:"lines"`
}

// Abyssal is one mutated item's stored estimate, or its queue state.
type Abyssal struct {
	ItemID   int64     `json:"item_id"`
	TypeID   int64     `json:"type_id"`
	State    string    `json:"state"` // priced, pending, not_mutated, failed, unpriced
	Estimate float64   `json:"estimate,omitempty"`
	Low      float64   `json:"estimate_low,omitempty"`
	High     float64   `json:"estimate_high,omitempty"`
	Basis    string    `json:"basis,omitempty"`
	Comps    int64     `json:"comparables,omitempty"`
	Score    float64   `json:"score,omitempty"`
	ScoredAt time.Time `json:"scored_at,omitzero"`
}

// KeyStatus is a key's own standing: its quota and today's use.
type KeyStatus struct {
	DailyQuota int64 `json:"daily_quota"`
	UsedToday  int64 `json:"used_today"`
	PerMinute  int   `json:"per_minute"`
	Burst      int   `json:"burst"`
	MaxBatch   int   `json:"max_batch"`
}
