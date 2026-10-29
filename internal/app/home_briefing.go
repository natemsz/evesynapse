package app

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/humanize"
	"evesynapse/internal/logging"
	"evesynapse/internal/markethistory"
)

// ---------------------------------------------------------------------------
// The Briefing home module — a since-you-last-looked
// digest computed entirely from cached data. A background worker
// keeps every snapshot this reads warm, so the facts are already
// here when the user arrives; the module only reads local tables
// and never calls ESI (the suite proves zero outbound calls).
//
// What qualifies, in three families:
//
//   - Standing states that need the user until they act: a
//     character needing re-linking (dead token / changed owner,
//     or the planetary scope missing from an older login), an
//     empty skill queue, stopped extractors, idle industry
//     slots, unread mail, undercut sell orders, and watched
//     prices past their threshold.
//   - Events inside the window: orders that expired, industry
//     jobs that finished and wait for delivery.
//   - The next 24 hours: orders expiring, skill queues ending,
//     extractors running dry, calendar events, courier deadlines.
//
// The window is anchored per account (users.last_briefing_at,
// migration 017): it starts at the previous anchor — the last 24
// hours when the module has never rendered, never more than 7
// days back — and advances after a normal home render that
// included the module. A home without the module never moves
// the anchor, and neither does a Customize pass.
// ---------------------------------------------------------------------------

const briefingCap = 12

// briefingWindow bounds: how far back the digest looks on its
// first-ever render, and how far back it ever looks.
const (
	briefingFirstWindow = 24 * time.Hour
	briefingMaxWindow   = 7 * 24 * time.Hour
	briefingAhead       = 24 * time.Hour
)

// Briefing rule ranks (lower surfaces first): things that block
// or cost the user lead, upcoming deadlines next, news last.
const (
	briefingRelink = iota
	briefingStoppedExtractor
	briefingOrderExpired
	briefingJobReady
	briefingUndercut
	briefingSkillEnding
	briefingNotTraining
	briefingIdleSlots
	briefingOrderExpiring
	briefingCourier
	briefingContract
	briefingCalendar
	briefingExtractorDry
	briefingWatchMove
	briefingMail
)

// briefingLine is one digest line. HTML is composed with the
// shared name-link helpers (own characters to their sheet, items
// to their page), so the template emits it as-is.
type briefingLine struct {
	html string
	Rank int
	At   time.Time // event time; chronological tiebreak inside a rank
	key  string    // character+rule dedupe key
	// Aggregation: count-bearing rules record their rule id, their
	// count and the character link, so buildBriefing can collapse
	// the same alert across briefingGroupAt or more characters into
	// one card (the flushIdle grouping, generalized).
	aggRule   string
	aggCount  int
	aggSub    int // secondary tally (planets under a pi-stopped line)
	aggPrefix string
	// aggName/aggHref: the character's plain name and the
	// character-scoped destination for what the rule warns about,
	// so an aggregated card's per-character segments link to the
	// thing itself (that character's planets, that character's
	// unread mail) instead of the character sheet.
	aggName string
	aggHref string
	// count is the card's top-right tally ("5 Pilots",
	// "3 Pilots / 17 Planets"), set on grouped and collapsed
	// lines; single-character lines leave it empty.
	count string
	// href is the line's own page: the character-scoped
	// destination its text already links (a view link, so
	// following it never moves the acting character). A stack
	// of lines links where its first line links.
	href string
}

type briefingWidget struct {
	Lines     []briefingLine
	HTMLLines []template.HTML // Lines rendered (template.HTML conversion of html)
	// Items are the same lines with their severity, for the
	// module's cards and its severity tabs; Counts tally them.
	Items []briefingItem
	// Stacks are the items grouped by kind, in the order their
	// first line ranks: the module renders one stack per kind
	// instead of a tile per line.
	Stacks []briefingStack
	// Dismissed are the lines the account has hidden that still
	// apply: they render only in the dismissed list (the modal),
	// never in the stacks, and drop away on their own once the
	// line stops applying.
	Dismissed  []briefingItem
	Counts     briefingCounts
	More       int    // qualifying lines hidden past the cap
	SinceLabel string // "Oct 2, 21:41" — window start as shown
}

// briefingItem is one rendered line and its severity.
type briefingItem struct {
	HTML     template.HTML
	Severity string // critical | warning | info
	Kind     string // icon category: mail | orders | skills | pi | ...
	Title    string // card headline for the digest rules that carry one
	Count    string // top-right tally on grouped/collapsed cards
	Key      string // the line's stable identity: its dismissal key
	Href     string // the line's own page (a view link where character-scoped)
}

// briefingStack is one kind's lines under one card: the stack's
// face links where its first line links, its tint follows its
// worst severity, and its count is how many lines it holds.
type briefingStack struct {
	Kind     string
	Title    string // the kind's name: Mail, Orders, Skills, ...
	Severity string // the worst severity among its lines
	Href     string // the first line's page
	Items    []briefingItem
}

// briefingKindTitle names a kind for its stack's face.
func briefingKindTitle(kind string) string {
	switch kind {
	case "mail":
		return "Mail"
	case "orders":
		return "Orders"
	case "skills":
		return "Skills"
	case "pi":
		return "Planetary"
	case "industry":
		return "Industry"
	case "contract":
		return "Contracts"
	case "calendar":
		return "Calendar"
	case "market":
		return "Market"
	case "link":
		return "Character Links"
	}
	return "Briefing"
}

// briefingSeverityRank orders severities worst first, for a
// stack's tint: critical outranks warning outranks info.
func briefingSeverityRank(sev string) int {
	switch sev {
	case "critical":
		return 0
	case "warning":
		return 1
	}
	return 2
}

// groupBriefingStacks folds items into one stack per kind,
// keeping the order the lines ranked in; a stack takes its worst
// severity and its first line's page.
func groupBriefingStacks(items []briefingItem) []briefingStack {
	var stacks []briefingStack
	byKind := map[string]int{}
	for _, it := range items {
		i, ok := byKind[it.Kind]
		if !ok {
			byKind[it.Kind] = len(stacks)
			stacks = append(stacks, briefingStack{
				Kind: it.Kind, Title: briefingKindTitle(it.Kind),
				Severity: it.Severity, Href: it.Href,
			})
			i = len(stacks) - 1
		}
		if briefingSeverityRank(it.Severity) < briefingSeverityRank(stacks[i].Severity) {
			stacks[i].Severity = it.Severity
		}
		stacks[i].Items = append(stacks[i].Items, it)
	}
	return stacks
}

// briefingItemOf renders one line as an item.
func briefingItemOf(l briefingLine) briefingItem {
	return briefingItem{
		HTML: template.HTML(l.html), Severity: briefingSeverity(l.Rank),
		Kind: briefingKind(l.Rank), Title: briefingTitle(l.Rank),
		Count: l.count, Key: l.key, Href: l.href,
	}
}

// briefingTitle is the card headline for the digest rules that
// carry one: the short name of the alert, shown big above the
// line's detail. Rules without an entry keep the bare line.
func briefingTitle(rank int) string {
	switch rank {
	case briefingNotTraining:
		return "Skill Queues Empty"
	case briefingStoppedExtractor:
		return "Extractors Paused"
	case briefingMail:
		return "Unread EVE Mails"
	case briefingSkillEnding:
		return "Queue Running Dry"
	}
	return ""
}

// briefingKind maps a rule rank to the icon category its card
// carries: mail, orders, industry and the rest of the surfaces
// the briefing speaks for.
func briefingKind(rank int) string {
	switch rank {
	case briefingRelink:
		return "link"
	case briefingStoppedExtractor, briefingExtractorDry:
		return "pi"
	case briefingOrderExpired, briefingOrderExpiring, briefingUndercut:
		return "orders"
	case briefingJobReady, briefingIdleSlots:
		return "industry"
	case briefingSkillEnding, briefingNotTraining:
		return "skills"
	case briefingCourier, briefingContract:
		return "contract"
	case briefingCalendar:
		return "calendar"
	case briefingWatchMove:
		return "market"
	case briefingMail:
		return "mail"
	}
	return "market"
}

type briefingCounts struct {
	All, Critical, Warning, Info int
}

// briefingSeverity groups the rule ranks into the three tabs the
// module filters by: what is already costing the user (critical),
// what wants doing soon (warning), and news (info).
func briefingSeverity(rank int) string {
	switch rank {
	case briefingRelink, briefingStoppedExtractor, briefingOrderExpired, briefingNotTraining:
		return "critical"
	case briefingCalendar, briefingWatchMove, briefingMail:
		return "info"
	}
	return "warning"
}

// briefingSince formatting for the module footer.
func briefingSinceLabel(since time.Time) string {
	return since.Format("Jan 2, 15:04")
}

// briefingSinceKey: char+rule dedupe key builder.
func briefingKey(charID int64, rule string) string {
	return fmt.Sprintf("%d|%s", charID, rule)
}

// briefCharLink is charLink for briefing lines: an own character's
// name links to their sheet as a view, so following the digest
// never moves the acting character. Anyone else's name keeps the
// shared link policy.
func briefCharLink(viewer map[int64]bool, charID int64, name string) template.HTML {
	if viewer[charID] {
		return template.HTML(fmt.Sprintf(`<a href="/character/?character=%d&view=1">%s</a>`, charID, template.HTMLEscapeString(name)))
	}
	return charLink(viewer, charID, name)
}

// briefingWindowStart resolves the digest window for one render:
// the stored anchor, 24h back on the first-ever render, and at
// most 7 days back however old the anchor is.
func (app *Application) briefingWindowStart(ctx context.Context, userID int64, now time.Time) time.Time {
	start := now.Add(-briefingFirstWindow)
	anchor, err := app.queries.GetUserBriefingAnchor(ctx, userID)
	if err != nil {
		logging.Errorf("home: briefing anchor for user %d: %v", userID, err)
		return start
	}
	if anchor.Valid && anchor.Time.Before(now) {
		start = anchor.Time
	}
	if start.Before(now.Add(-briefingMaxWindow)) {
		start = now.Add(-briefingMaxWindow)
	}
	return start
}

// briefingAnchorStep is the finest anchor movement worth a
// database write. The digest label renders the anchor to the
// minute, so an anchor that would move by less than this leaves
// the window (and every line in it) unchanged; the advance is
// then a render-path UPDATE whose only effect is queueing
// behind every other writer. Repeat home renders inside the
// step skip the write entirely.
const briefingAnchorStep = time.Minute

// advanceBriefingAnchor moves the account's window anchor, but
// only when the stored anchor would actually move: at least a
// full briefingAnchorStep behind this render (or never set, as
// on the first run). Only a normal home render that
// included the module calls this.
func (app *Application) advanceBriefingAnchor(ctx context.Context, userID int64, now time.Time) {
	anchor, err := app.queries.GetUserBriefingAnchor(ctx, userID)
	if err != nil {
		logging.Errorf("home: briefing anchor for user %d: %v", userID, err)
		return
	}
	if anchor.Valid && !anchor.Time.Before(now.Add(-briefingAnchorStep)) {
		// The stored anchor already sits within a step of this
		// render (or ahead of it, under clock skew): advancing
		// it would change nothing the digest can show, so skip
		// the write.
		return
	}
	if err := app.queries.SetUserBriefingAnchor(ctx, db.SetUserBriefingAnchorParams{
		LastBriefingAt: timeSet(now.UTC()),
		ID:             userID,
	}); err != nil {
		logging.Errorf("home: advance briefing anchor for user %d: %v", userID, err)
	}
}

// briefingTopicBuiltIn reports whether a built-in module declares the
// briefing topic: the line families a module owns are produced only
// where that module is built in. The builders stay here in core; the
// gate is the module's BriefingTopics declaration, read from the
// registry like Widgets and NotifyKinds.
func briefingTopicBuiltIn(id string) bool {
	for _, m := range registeredModules {
		for _, t := range m.BriefingTopics {
			if t.ID == id {
				return true
			}
		}
	}
	return false
}

func (app *Application) buildBriefing(ctx context.Context, bundles []*charSnaps, since, now time.Time, shared *homeMarketShared) *briefingWidget {
	w := &briefingWidget{SinceLabel: briefingSinceLabel(since)}
	if len(bundles) == 0 {
		return w
	}
	viewer := make(map[int64]bool, len(bundles))
	names := make(map[int64]string, len(bundles))
	for _, b := range bundles {
		viewer[b.ch.CharacterID] = true
		names[b.ch.CharacterID] = b.ch.Name
	}

	bb := &briefingBuilder{
		app:    app,
		since:  since,
		now:    now,
		viewer: viewer,
		seen:   map[string]bool{},
		// Industry slot bases need the four slot skills' type IDs,
		// resolved from the local static data by name (type IDs are
		// CCP's to change; names are how the skill catalog speaks).
		slotSkillIDs: app.slotSkillIDs(ctx),
	}
	for _, b := range bundles {
		prefix := string(briefCharLink(viewer, b.ch.CharacterID, b.ch.Name))
		bb.linkLines(ctx, b, prefix)
		bb.skillLines(ctx, b, prefix)
		bb.industryLines(ctx, b, prefix)
		if briefingTopicBuiltIn("orders") {
			bb.orderLines(ctx, b, prefix)
		}
		bb.planetLines(ctx, b, prefix)
		// What a module owns is told only where the module is built in.
		if briefingTopicBuiltIn("contracts") {
			bb.contractLines(b, prefix)
		}
		if briefingTopicBuiltIn("calendar") {
			bb.calendarLines(b, prefix)
		}
		if briefingTopicBuiltIn("mail") {
			bb.mailLines(b, prefix)
		}
	}
	bb.flushIdle()
	lines := bb.lines

	// --- Account-level market lines: undercut sell orders and
	// watched-price moves, from the worker's stored verdicts and
	// price history (the Needs-attention feed's own sources). ----
	lines = append(lines, app.briefingMarketLines(ctx, bundles, viewer, names, shared)...)
	lines = aggregateBriefing(lines)

	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].Rank != lines[j].Rank {
			return lines[i].Rank < lines[j].Rank
		}
		return lines[i].At.Before(lines[j].At)
	})
	// Dismissals: a line the account hid moves out of the stacks
	// into the dismissed list while it still applies — hiding is
	// not deleting, the line is recomputed on every render. A
	// stored dismissal whose line no longer applies is pruned
	// here, so the stored set only ever holds live hides.
	userID := app.userID(ctx)
	dismissed := map[string]bool{}
	if userID != 0 {
		keys, err := app.queries.ListBriefingDismissals(ctx, userID)
		if err != nil {
			logging.Errorf("home: briefing dismissals for user %d: %v", userID, err)
		}
		for _, k := range keys {
			dismissed[k] = true
		}
	}
	if len(dismissed) > 0 {
		live := map[string]bool{}
		for _, l := range lines {
			if l.key != "" {
				live[l.key] = true
			}
		}
		for k := range dismissed {
			if live[k] {
				continue
			}
			if err := app.queries.DeleteBriefingDismissal(ctx, db.DeleteBriefingDismissalParams{UserID: userID, LineKey: k}); err != nil {
				logging.Errorf("home: prune briefing dismissal for user %d: %v", userID, err)
			}
			delete(dismissed, k)
		}
	}
	var visible []briefingLine
	for _, l := range lines {
		if l.key != "" && dismissed[l.key] {
			w.Dismissed = append(w.Dismissed, briefingItemOf(l))
			continue
		}
		visible = append(visible, l)
	}
	if len(visible) > briefingCap {
		w.More = len(visible) - briefingCap
		visible = visible[:briefingCap]
	}
	for _, l := range visible {
		w.Lines = append(w.Lines, l)
		w.HTMLLines = append(w.HTMLLines, template.HTML(l.html))
		it := briefingItemOf(l)
		w.Items = append(w.Items, it)
		switch it.Severity {
		case "critical":
			w.Counts.Critical++
		case "warning":
			w.Counts.Warning++
		default:
			w.Counts.Info++
		}
		w.Counts.All++
	}
	w.Stacks = groupBriefingStacks(w.Items)
	return w
}

// handleBriefingDismiss hides one briefing line for the account.
// The line is not deleted: it keeps being computed and renders in
// the briefing's dismissed list while it applies.
func (app *Application) handleBriefingDismiss(w http.ResponseWriter, r *http.Request) {
	app.briefingDismissal(w, r, true)
}

// handleBriefingRestore un-hides one briefing line.
func (app *Application) handleBriefingRestore(w http.ResponseWriter, r *http.Request) {
	app.briefingDismissal(w, r, false)
}

func (app *Application) briefingDismissal(w http.ResponseWriter, r *http.Request, dismiss bool) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if err := r.ParseForm(); err == nil && userID != 0 {
		if key := r.FormValue("key"); key != "" {
			var err error
			if dismiss {
				err = app.queries.DismissBriefingLine(ctx, db.DismissBriefingLineParams{UserID: userID, LineKey: key})
			} else {
				err = app.queries.DeleteBriefingDismissal(ctx, db.DeleteBriefingDismissalParams{UserID: userID, LineKey: key})
			}
			if err != nil {
				logging.Errorf("home: briefing dismissal change for user %d: %v", userID, err)
			}
		}
	}
	if r.Header.Get("X-Requested-With") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// briefingAggSummary renders one collapsed card's headline for a
// rule: the account-wide total first, then the section link and
// the per-character segments the caller joins in.
func briefingAggSummary(rule string, total int) (string, bool) {
	switch rule {
	case "pi-stopped":
		return fmt.Sprintf("%d extractor%s have stopped", total, pluralSuffix(total)) +
			`. <a href="/planets/">Colonies</a>: `, true
	case "mail":
		return fmt.Sprintf("%d unread mail%s", total, pluralSuffix(total)) +
			`. <a href="/mail/">Mail</a>: `, true
	case "order-expired":
		return fmt.Sprintf("%d market order%s expired since you last looked", total, pluralSuffix(total)) +
			`. <a href="/orders/">Orders</a>: `, true
	case "order-expiring":
		return fmt.Sprintf("%d market order%s expire%s within a day", total, pluralSuffix(total), pluralVerb(total)) +
			`. <a href="/orders/">Orders</a>: `, true
	case "job-ready":
		return fmt.Sprintf("%d industry job%s finished and waiting for delivery", total, pluralSuffix(total)) +
			`. <a href="/industry/jobs/">My Jobs</a>: `, true
	case "contract-waiting":
		return fmt.Sprintf("%d contract%s waiting for your answer", total, pluralSuffix(total)) +
			`. <a href="/contracts/">Contracts</a>: `, true
	}
	return "", false
}

// briefingAggHref is the section page an aggregated line's card
// links to: the summary's own section link, without a character.
func briefingAggHref(rule string) string {
	switch rule {
	case "pi-stopped":
		return "/planets/"
	case "mail":
		return "/mail/"
	case "order-expired", "order-expiring":
		return "/orders/"
	case "job-ready":
		return "/industry/jobs/"
	case "contract-waiting":
		return "/contracts/"
	}
	return "/"
}

// aggregateBriefing collapses the same count-bearing alert across
// briefingGroupAt or more characters into one card: the total, then
// each character's name and count (the first four; the rest fold
// into "and N others", their counts already inside the total).
// Smaller spreads keep their detailed per-character lines, the same
// threshold flushIdle uses for empty skill queues.
func aggregateBriefing(lines []briefingLine) []briefingLine {
	groups := map[string][]int{}
	for i, l := range lines {
		if l.aggRule != "" {
			groups[l.aggRule] = append(groups[l.aggRule], i)
		}
	}
	drop := map[int]bool{}
	merged := map[int]briefingLine{}
	for rule, idxs := range groups {
		if len(idxs) < briefingGroupAt {
			continue
		}
		if _, ok := briefingAggSummary(rule, 0); !ok {
			continue
		}
		total := 0
		subTotal := 0
		latest := time.Time{}
		segs := make([]string, 0, len(idxs))
		for n, i := range idxs {
			l := lines[i]
			total += l.aggCount
			subTotal += l.aggSub
			if l.At.After(latest) {
				latest = l.At
			}
			if n < 4 {
				seg := l.aggPrefix
				if l.aggHref != "" {
					seg = fmt.Sprintf("<a href=\"%s\">%s</a>", l.aggHref, template.HTMLEscapeString(l.aggName))
				}
				segs = append(segs, fmt.Sprintf("%s (%d)", seg, l.aggCount))
			}
		}
		if len(idxs) > 4 {
			segs = append(segs, fmt.Sprintf("and %d more", len(idxs)-4))
		}
		summary, _ := briefingAggSummary(rule, total)
		count := fmt.Sprintf("%d Total", total)
		if rule == "pi-stopped" {
			count = fmt.Sprintf("%d Pilots / %d Planets", len(idxs), subTotal)
		}
		first := lines[idxs[0]]
		merged[idxs[0]] = briefingLine{
			html: summary + strings.Join(segs, ", "),
			Rank: first.Rank, At: latest, key: "agg-" + rule,
			count: count, href: briefingAggHref(rule),
		}
		for _, i := range idxs[1:] {
			drop[i] = true
		}
	}
	if len(merged) == 0 {
		return lines
	}
	out := make([]briefingLine, 0, len(lines))
	for i, l := range lines {
		if drop[i] {
			continue
		}
		if m, ok := merged[i]; ok {
			out = append(out, m)
			continue
		}
		out = append(out, l)
	}
	return out
}

// briefingBuilder collects one account's per-character digest lines.
// Each rule is a method that looks at one character's snapshots and
// adds the lines it finds; add drops a second line for the same
// character and rule.
type briefingBuilder struct {
	app        *Application
	since, now time.Time
	// viewer is the account's own characters, for name links.
	viewer       map[int64]bool
	slotSkillIDs map[string]int64

	lines []briefingLine
	seen  map[string]bool

	// idle holds the characters with an empty skill queue until every
	// character is seen: three or more become one grouped line.
	idle []idleLine
}

// idleLine is one character with nothing training.
type idleLine struct {
	id     int64
	name   string
	prefix string
	at     time.Time
}

// briefingGroupAt is how many of the same alert collapse into one line.
const briefingGroupAt = 3

// flushIdle writes the empty-queue lines: one per character, or one
// line naming them all once there are briefingGroupAt or more.
func (bb *briefingBuilder) flushIdle() {
	if len(bb.idle) >= briefingGroupAt {
		names := make([]string, 0, len(bb.idle))
		latest := time.Time{}
		for _, l := range bb.idle {
			names = append(names, fmt.Sprintf("<a href=\"/character/?character=%d&view=1#skills\">%s</a>", l.id, template.HTMLEscapeString(l.name)))
			if l.at.After(latest) {
				latest = l.at
			}
		}
		bb.addCounted("not-training-group", briefingNotTraining, latest,
			template.HTML(fmt.Sprintf("%d skill queues empty — %s. <a href=\"/characters/\">Characters</a>", len(bb.idle), strings.Join(names, ", "))),
			fmt.Sprintf("%d Pilots", len(bb.idle)), "/characters/")
		return
	}
	for _, l := range bb.idle {
		bb.add(briefingKey(l.id, "not-training"), briefingNotTraining, l.at,
			template.HTML(fmt.Sprintf("%s isn't training — the skill queue is empty. <a href=\"/character/?character=%d&view=1#skills\">Skills</a>", l.prefix, l.id)),
			fmt.Sprintf("/character/?character=%d&view=1#skills", l.id))
	}
}

func (bb *briefingBuilder) add(key string, rank int, at time.Time, html template.HTML, href string) {
	bb.addCounted(key, rank, at, html, "", href)
}

// addCounted is add for a line that also carries the card tally
// shown at its top right (the grouped idle-queue line).
func (bb *briefingBuilder) addCounted(key string, rank int, at time.Time, html template.HTML, count, href string) {
	if bb.seen[key] {
		return
	}
	bb.seen[key] = true
	bb.lines = append(bb.lines, briefingLine{html: string(html), Rank: rank, At: at, key: key, count: count, href: href})
}

// addAgg is add for a count-bearing rule: it also records the rule,
// the count and the character link so the line can take part in
// cross-character aggregation (see aggregateBriefing). name and
// href are the character's plain name and the character-scoped
// destination for the warning, for the aggregated segments.
func (bb *briefingBuilder) addAgg(key string, rank int, at time.Time, html template.HTML, rule string, count, sub int, prefix, name, href string) {
	if bb.seen[key] {
		return
	}
	bb.seen[key] = true
	bb.lines = append(bb.lines, briefingLine{html: string(html), Rank: rank, At: at, key: key,
		aggRule: rule, aggCount: count, aggSub: sub, aggPrefix: prefix, aggName: name, aggHref: href, href: href})
}

// linkLines: the standing lines about the link itself (a character
// that needs re-linking, or whose login predates a scope).
func (bb *briefingBuilder) linkLines(ctx context.Context, b *charSnaps, prefix string) {
	app := bb.app
	if b.ch.LinkState != "" && b.ch.LinkState != linkStateOK {
		reason := "the saved sign-in stopped working"
		if b.ch.LinkState == linkStateOwnerChanged {
			reason = "the character changed accounts"
		}
		bb.add(briefingKey(b.ch.CharacterID, "relink"), briefingRelink, time.Time{},
			template.HTML(fmt.Sprintf("%s needs re-linking — %s. <a href=\"/characters/\">Characters</a>", prefix, reason)),
			"/characters/")
	} else if app.piNotEnabled(ctx, b.ch) {
		// A healthy link whose login predates the planetary
		// scope: colonies stay dark until a fresh sign-in.
		bb.add(briefingKey(b.ch.CharacterID, "pi-scope"), briefingRelink, time.Time{},
			template.HTML(fmt.Sprintf("%s — planetary industry stays off until they sign in again. <a href=\"/planets/?character=%d&view=1\">Planets</a>", prefix, b.ch.CharacterID)),
			fmt.Sprintf("/planets/?character=%d&view=1", b.ch.CharacterID))
	}
}

// skillLines: nothing training, or the queue ends soon.
func (bb *briefingBuilder) skillLines(ctx context.Context, b *charSnaps, prefix string) {
	app, now := bb.app, bb.now
	if b.queueKnown {
		training, finishedAt := queueState(b.queue, now)
		switch {
		case !training:
			bb.idle = append(bb.idle, idleLine{id: b.ch.CharacterID, name: b.ch.Name, prefix: prefix, at: finishedAt})
		default:
			if head := queueHead(b.queue, now); head != nil {
				if finish, ok := parseRFC3339(lastQueueFinish(b.queue)); ok && finish.After(now) && finish.Before(now.Add(briefingAhead)) {
					skillID := head.SkillID
					skillName := app.typeNameOrID(ctx, skillID)
					bb.add(briefingKey(b.ch.CharacterID, "queue-ending"), briefingSkillEnding, finish,
						template.HTML(fmt.Sprintf("%s finishes %s %s in %s — queue runs dry after that. <a href=\"/character/?character=%d&view=1#skills\">Skills</a>",
							prefix, string(itemLink(skillID, skillName)), esi.RomanLevel(head.FinishedLevel),
							humanDuration(time.Until(finish)), b.ch.CharacterID)),
						fmt.Sprintf("/character/?character=%d&view=1#skills", b.ch.CharacterID))
				}
			}
		}
	}
}

// industryLines: finished jobs waiting for delivery, and idle slots.
func (bb *briefingBuilder) industryLines(ctx context.Context, b *charSnaps, prefix string) {
	app, since, now, slotSkillIDs := bb.app, bb.since, bb.now, bb.slotSkillIDs
	if b.jobsKnown {
		readyN := 0
		var readyAt time.Time
		var readyItem template.HTML
		activeManuf, activeResearch := 0, 0
		for _, j := range b.jobs {
			switch j.Status {
			case "ready":
				done, ok := industryDoneAt(j)
				if ok && !done.Before(since) && !done.After(now) {
					readyN++
					if readyAt.IsZero() || done.Before(readyAt) {
						readyAt = done
						nameID := j.ProductTypeID
						if nameID == 0 {
							nameID = j.BlueprintTypeID
						}
						readyItem = itemLink(nameID, app.typeNameOrID(ctx, nameID))
					}
				}
			case "active", "paused":
				switch j.ActivityID {
				case 1, 11:
					activeManuf++
				case 2, 3, 4, 5, 8:
					activeResearch++
				}
			}
		}
		if readyN > 0 {
			line := template.HTML(fmt.Sprintf("%s has %d industry job%s finished and waiting for delivery", prefix, readyN, pluralSuffix(readyN)))
			if readyItem != "" {
				line += template.HTML(fmt.Sprintf(" — first up: %s", string(readyItem)))
			}
			line += template.HTML(fmt.Sprintf(". <a href=\"/industry/jobs/?character=%d&view=1\">My Jobs</a>", b.ch.CharacterID))
			bb.addAgg(briefingKey(b.ch.CharacterID, "job-ready"), briefingJobReady, readyAt, line, "job-ready", readyN, 0, prefix,
				b.ch.Name, fmt.Sprintf("/industry/jobs/?character=%d&view=1", b.ch.CharacterID))
		}

		if b.skillsKnown && len(b.jobs) > 0 {
			levels := briefingSkillLevels(b.skills)
			manufMax := 1 + levels[slotSkillIDs["Mass Production"]] + levels[slotSkillIDs["Advanced Mass Production"]]
			researchMax := 1 + levels[slotSkillIDs["Laboratory Operation"]] + levels[slotSkillIDs["Advanced Laboratory Operation"]]
			if idle := manufMax - activeManuf; idle > 0 {
				bb.add(briefingKey(b.ch.CharacterID, "idle-manufacturing"), briefingIdleSlots, time.Time{},
					template.HTML(fmt.Sprintf("%s has %d of %d manufacturing slots idle. <a href=\"/industry/jobs/?character=%d&view=1\">My Jobs</a>", prefix, idle, manufMax, b.ch.CharacterID)),
					fmt.Sprintf("/industry/jobs/?character=%d&view=1", b.ch.CharacterID))
			}
			if idle := researchMax - activeResearch; idle > 0 {
				bb.add(briefingKey(b.ch.CharacterID, "idle-research"), briefingIdleSlots, time.Time{},
					template.HTML(fmt.Sprintf("%s has %d of %d research slots idle. <a href=\"/industry/jobs/?character=%d&view=1\">My Jobs</a>", prefix, idle, researchMax, b.ch.CharacterID)),
					fmt.Sprintf("/industry/jobs/?character=%d&view=1", b.ch.CharacterID))
			}
		}
	}
}

// orderLines: market orders that expired inside the window, and
// orders expiring within a day.
func (bb *briefingBuilder) orderLines(ctx context.Context, b *charSnaps, prefix string) {
	app, since, now := bb.app, bb.since, bb.now
	if b.orderHistKnown {
		expiredN := 0
		var expiredAt time.Time
		var expiredItem template.HTML
		for _, e := range b.orderHist {
			if e.State != "expired" {
				continue
			}
			expiry, ok := orderExpiry(e.CharOrder)
			if !ok || expiry.Before(since) || expiry.After(now) {
				continue
			}
			expiredN++
			if expiredAt.IsZero() || expiry.After(expiredAt) {
				expiredAt = expiry
				expiredItem = itemLink(e.TypeID, app.typeNameOrID(ctx, e.TypeID))
			}
		}
		if expiredN > 0 {
			line := template.HTML(fmt.Sprintf("%s — %d market order%s expired since you last looked", prefix, expiredN, pluralSuffix(expiredN)))
			if expiredItem != "" {
				line += template.HTML(fmt.Sprintf(" — most recently %s", string(expiredItem)))
			}
			line += template.HTML(fmt.Sprintf(". <a href=\"/orders/?character=%d&view=1\">Orders</a>", b.ch.CharacterID))
			bb.addAgg(briefingKey(b.ch.CharacterID, "order-expired"), briefingOrderExpired, expiredAt, line, "order-expired", expiredN, 0, prefix,
				b.ch.Name, fmt.Sprintf("/orders/?character=%d&view=1", b.ch.CharacterID))
		}
	}
	if b.ordersKnown {
		expiringN := 0
		var expiringAt time.Time
		var expiringItem template.HTML
		for _, o := range b.orders {
			expiry, ok := orderExpiry(o)
			if !ok || expiry.Before(now) || expiry.After(now.Add(briefingAhead)) {
				continue
			}
			expiringN++
			if expiringAt.IsZero() || expiry.Before(expiringAt) {
				expiringAt = expiry
				expiringItem = itemLink(o.TypeID, app.typeNameOrID(ctx, o.TypeID))
			}
		}
		if expiringN > 0 {
			line := template.HTML(fmt.Sprintf("%s — %d market order%s expire%s within a day", prefix, expiringN, pluralSuffix(expiringN), pluralVerb(expiringN)))
			if expiringItem != "" {
				line += template.HTML(fmt.Sprintf(" — soonest: %s in %s", string(expiringItem), humanDuration(time.Until(expiringAt))))
			}
			line += template.HTML(fmt.Sprintf(". <a href=\"/orders/?character=%d&view=1\">Orders</a>", b.ch.CharacterID))
			bb.addAgg(briefingKey(b.ch.CharacterID, "order-expiring"), briefingOrderExpiring, expiringAt, line, "order-expiring", expiringN, 0, prefix,
				b.ch.Name, fmt.Sprintf("/orders/?character=%d&view=1", b.ch.CharacterID))
		}
	}
}

// planetLines: extractors that have stopped, and ones about to run
// dry.
func (bb *briefingBuilder) planetLines(ctx context.Context, b *charSnaps, prefix string) {
	app, now := bb.app, bb.now
	if b.planetsKnown {
		stoppedN, stoppedPlanets := 0, 0
		planetSeen := map[int64]bool{}
		dryN := 0
		var dryAt time.Time
		var dryPlanet string
		for _, colony := range b.colonies {
			layout := b.layouts[colony.PlanetID]
			if layout == nil {
				continue
			}
			for _, ex := range layoutExtractors(*layout) {
				if !ex.ExpiryOK {
					continue
				}
				switch {
				case ex.Expiry.Before(now):
					stoppedN++
					if !planetSeen[colony.PlanetID] {
						planetSeen[colony.PlanetID] = true
						stoppedPlanets++
					}
				case ex.Expiry.Before(now.Add(briefingAhead)):
					dryN++
					if dryAt.IsZero() || ex.Expiry.Before(dryAt) {
						dryAt = ex.Expiry
						dryPlanet = app.planetDisplayName(ctx, colony.PlanetID)
					}
				}
			}
		}
		if stoppedN > 0 {
			bb.addAgg(briefingKey(b.ch.CharacterID, "pi-stopped"), briefingStoppedExtractor, time.Time{},
				template.HTML(fmt.Sprintf("%s — %d extractor%s have stopped across %d planet%s. <a href=\"/planets/?character=%d&view=1\">Colonies</a>",
					prefix, stoppedN, pluralSuffix(stoppedN), stoppedPlanets, pluralSuffix(stoppedPlanets), b.ch.CharacterID)),
				"pi-stopped", stoppedN, stoppedPlanets, prefix,
				b.ch.Name, fmt.Sprintf("/planets/?character=%d&view=1", b.ch.CharacterID))
		}
		if dryN > 0 {
			bb.add(briefingKey(b.ch.CharacterID, "pi-dry"), briefingExtractorDry, dryAt,
				template.HTML(fmt.Sprintf("%s — an extractor on %s runs dry in %s. <a href=\"/planets/?character=%d&view=1\">Colonies</a>",
					prefix, template.HTMLEscapeString(dryPlanet), humanDuration(time.Until(dryAt)), b.ch.CharacterID)),
				fmt.Sprintf("/planets/?character=%d&view=1", b.ch.CharacterID))
		}
	}
}

// contractLines: contracts waiting for an answer, and courier
// deadlines.
func (bb *briefingBuilder) contractLines(b *charSnaps, prefix string) {
	now := bb.now
	if b.contractsKnown {
		waitingN := 0
		var waitingAt time.Time
		for _, c := range b.contracts {
			switch {
			case c.Status == "outstanding" && c.AssigneeID == b.ch.CharacterID && c.AcceptorID == 0:
				waitingN++
				if at, ok := parseRFC3339(c.DateExpired); ok && (waitingAt.IsZero() || at.Before(waitingAt)) {
					waitingAt = at
				}
			case c.Status == "in_progress" && c.Type == "courier" && c.AcceptorID == b.ch.CharacterID:
				deadline, ok := courierDeadline(c)
				if !ok || deadline.After(now.Add(briefingAhead)) {
					continue
				}
				when := "due in " + humanDuration(time.Until(deadline))
				if deadline.Before(now) {
					when = "overdue"
				}
				title := c.Title
				if title == "" {
					title = "courier run"
				}
				bb.add(briefingKey(b.ch.CharacterID, fmt.Sprintf("courier-%d", c.ContractID)), briefingCourier, deadline,
					template.HTML(fmt.Sprintf("%s — courier: %s is %s. <a href=\"/contracts/?character=%d&view=1\">Contracts</a>",
						prefix, template.HTMLEscapeString(title), when, b.ch.CharacterID)),
					fmt.Sprintf("/contracts/?character=%d&view=1", b.ch.CharacterID))
			}
		}
		if waitingN > 0 {
			bb.addAgg(briefingKey(b.ch.CharacterID, "contract-waiting"), briefingContract, waitingAt,
				template.HTML(fmt.Sprintf("%s — %d contract%s waiting for your answer. <a href=\"/contracts/?character=%d&view=1\">Contracts</a>",
					prefix, waitingN, pluralSuffix(waitingN), b.ch.CharacterID)),
				"contract-waiting", waitingN, 0, prefix,
				b.ch.Name, fmt.Sprintf("/contracts/?character=%d&view=1", b.ch.CharacterID))
		}
	}
}

// calendarLines: events inside the next day.
func (bb *briefingBuilder) calendarLines(b *charSnaps, prefix string) {
	now := bb.now
	if b.calendarKnown {
		for _, ev := range b.calendar {
			at, ok := parseRFC3339(ev.EventDate)
			if !ok || at.Before(now.Add(-30*time.Minute)) || at.After(now.Add(briefingAhead)) {
				continue
			}
			title := ev.Title
			if title == "" {
				title = "Calendar event"
			}
			line := template.HTML(fmt.Sprintf("%s — “%s” starts in %s. <a href=\"/calendar/?character=%d&view=1\">Calendar</a>",
				prefix, template.HTMLEscapeString(title), humanDuration(time.Until(at)), b.ch.CharacterID))
			if at.Before(now) {
				line = template.HTML(fmt.Sprintf("%s — “%s” has started. <a href=\"/calendar/?character=%d&view=1\">Calendar</a>",
					prefix, template.HTMLEscapeString(title), b.ch.CharacterID))
			}
			bb.add(briefingKey(b.ch.CharacterID, fmt.Sprintf("calendar-%d", ev.EventID)), briefingCalendar, at, line,
				fmt.Sprintf("/calendar/?character=%d&view=1", b.ch.CharacterID))
		}
	}
}

// mailLines: the unread count and the newest sender.
func (bb *briefingBuilder) mailLines(b *charSnaps, prefix string) {
	app, viewer := bb.app, bb.viewer
	if b.mailLabels != nil && b.mailLabels.TotalUnreadCount > 0 {
		line := template.HTML(fmt.Sprintf("%s has %d unread mail%s. <a href=\"/mail/?character=%d&view=1\">Mail</a>",
			prefix, b.mailLabels.TotalUnreadCount, pluralSuffix(int(b.mailLabels.TotalUnreadCount)), b.ch.CharacterID))
		if b.mailKnown {
			if hdr, ok := newestUnreadHeader(b.mail); ok {
				if sender, found := app.esi.CachedCharacterName(hdr.From); found && sender != "" {
					line = template.HTML(fmt.Sprintf("%s has %d unread mail%s — newest from %s. <a href=\"/mail/?character=%d&view=1\">Mail</a>",
						prefix, b.mailLabels.TotalUnreadCount, pluralSuffix(int(b.mailLabels.TotalUnreadCount)),
						string(briefCharLink(viewer, hdr.From, sender)), b.ch.CharacterID))
				}
			}
		}
		bb.addAgg(briefingKey(b.ch.CharacterID, "mail"), briefingMail, time.Time{}, line, "mail", int(b.mailLabels.TotalUnreadCount), 0, prefix,
			b.ch.Name, fmt.Sprintf("/mail/?character=%d&view=1", b.ch.CharacterID))
	}
}

// briefingMarketLines builds the account-level market lines:
// undercut sell orders (one line per character, their worst
// example named) and watchlist moves past the saved threshold
// (one per watched item), from stored verdicts and history.
func (app *Application) briefingMarketLines(ctx context.Context, bundles []*charSnaps, viewer map[int64]bool, names map[int64]string, shared *homeMarketShared) []briefingLine {
	if len(bundles) == 0 {
		return nil
	}
	var lines []briefingLine

	// The render's shared rows: the same order-health verdicts
	// and watchlist entries the market and watchlist widgets
	// read, fetched once for the whole page.
	var health []db.OrderHealth
	if briefingTopicBuiltIn("orders") {
		health = shared.orderHealth(ctx)
	}
	if health != nil {
		perChar := map[int64]int{}
		example := map[int64]db.OrderHealth{}
		for _, h := range health {
			if h.Status != "undercut_station" && h.Status != "undercut_region" {
				continue
			}
			perChar[h.CharacterID]++
			if _, ok := example[h.CharacterID]; !ok {
				example[h.CharacterID] = h
			}
		}
		charIDs := make([]int64, 0, len(perChar))
		for id := range perChar {
			charIDs = append(charIDs, id)
		}
		sort.Slice(charIDs, func(i, j int) bool { return names[charIDs[i]] < names[charIDs[j]] })
		for _, id := range charIDs {
			ex := example[id]
			n := perChar[id]
			line := template.HTML(fmt.Sprintf("%s has %d sell order%s undercut right now — %s is no longer the best price. <a href=\"/orders/?character=%d&view=1\">Orders</a>",
				string(briefCharLink(viewer, id, names[id])),
				n, pluralSuffix(n),
				string(itemLink(ex.TypeID, app.typeNameOrID(ctx, ex.TypeID))), id))
			lines = append(lines, briefingLine{html: string(line), Rank: briefingUndercut, At: ex.ComputedAt,
				key: briefingKey(id, "undercut"), href: fmt.Sprintf("/orders/?character=%d&view=1", id)})
		}
	}

	for _, e := range shared.watchlist(ctx) {
		rows := app.recentHistoryRows(ctx, marketRegionForType(e.TypeID, e.RegionID), e.TypeID, markethistory.ChartRows)
		pct, ok := markethistory.ChangePct(rows, 7)
		if !ok || absFloat(pct) < e.ThresholdPct {
			continue
		}
		line := template.HTML(fmt.Sprintf("%s moved %s over 7 days in %s — past your %s watch. <a href=\"/market/\">Watchlist</a>",
			string(itemLink(e.TypeID, app.typeNameOrID(ctx, e.TypeID))),
			markethistory.ChangeDirection(pct),
			template.HTMLEscapeString(app.marketRegionLabel(ctx, marketRegionForType(e.TypeID, e.RegionID))),
			markethistory.FormatChangePct(e.ThresholdPct)))
		lines = append(lines, briefingLine{html: string(line), Rank: briefingWatchMove,
			key: fmt.Sprintf("watch|%d|%d", e.TypeID, e.RegionID), href: "/market/"})
	}
	return lines
}

// slotSkillIDs resolves the four industry slot skills' type IDs
// from the local static data, by name. Missing names (static
// data not imported yet) simply keep the slot rules quiet.
func (app *Application) slotSkillIDs(ctx context.Context) map[string]int64 {
	out := map[string]int64{
		"Mass Production":               0,
		"Advanced Mass Production":      0,
		"Laboratory Operation":          0,
		"Advanced Laboratory Operation": 0,
	}
	names := make([]string, 0, len(out))
	for name := range out {
		names = append(names, name)
	}
	rows, err := app.queries.ListSDETypesByNames(ctx, names)
	if err != nil {
		return out
	}
	for _, row := range rows {
		if _, wanted := out[row.Name]; wanted {
			out[row.Name] = row.TypeID
		}
	}
	return out
}

// briefingSkillLevels maps a trained-skills snapshot to skill ID
// → active level (the level its bonuses run at).
func briefingSkillLevels(skills esi.Skills) map[int64]int {
	levels := make(map[int64]int, len(skills.Skills))
	for _, s := range skills.Skills {
		levels[s.SkillID] = s.ActiveSkillLevel
	}
	return levels
}

// industryDoneAt is when an industry job finished: the recorded
// completion when present, else its scheduled end.
func industryDoneAt(j esi.IndustryJob) (time.Time, bool) {
	if t, ok := parseRFC3339(j.CompletedDate); ok {
		return t, true
	}
	return parseRFC3339(j.EndDate)
}

// lastQueueFinish is the last finish date in a skill queue.
func lastQueueFinish(queue esi.Skillqueue) string {
	last := ""
	for _, e := range queue {
		if e.FinishDate > last {
			last = e.FinishDate
		}
	}
	return last
}

// courierDeadline: a courier's clock starts at acceptance and
// runs DaysToComplete days.
func courierDeadline(c esi.Contract) (time.Time, bool) {
	accepted, ok := parseRFC3339(c.DateAccepted)
	if !ok || c.DaysToComplete <= 0 {
		return time.Time{}, false
	}
	return accepted.Add(time.Duration(c.DaysToComplete) * 24 * time.Hour), true
}

// newestUnreadHeader finds the newest unread mail header.
func newestUnreadHeader(headers esi.MailHeaders) (esi.MailHeader, bool) {
	var best esi.MailHeader
	var bestAt time.Time
	found := false
	for _, h := range headers {
		if h.IsRead {
			continue
		}
		at, err := time.Parse(time.RFC3339, h.Timestamp)
		if err != nil {
			continue
		}
		if !found || at.After(bestAt) {
			best, bestAt, found = h, at, true
		}
	}
	return best, found
}

// pluralSuffix: "" for one, "s" otherwise ("3 extractors").
func pluralSuffix(n int) string { return humanize.PluralSuffix(n) }

// pluralVerb: "s" for one, "" otherwise ("1 order expires").
func pluralVerb(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}
