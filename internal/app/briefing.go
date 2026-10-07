package app

import (
	"context"
	"fmt"
	"html/template"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Phase 6: the Briefing home module — a since-you-last-looked
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
}

type briefingWidget struct {
	Lines      []briefingLine
	HTMLLines  []template.HTML // Lines rendered (template.HTML conversion of html)
	More       int             // qualifying lines hidden past the cap
	SinceLabel string          // "Oct 2, 21:41" — window start as shown
}

// briefingSince formatting for the module footer.
func briefingSinceLabel(since time.Time) string {
	return since.Format("Jan 2, 15:04")
}

// briefingSinceKey: char+rule dedupe key builder.
func briefingKey(charID int64, rule string) string {
	return fmt.Sprintf("%d|%s", charID, rule)
}

// briefingWindowStart resolves the digest window for one render:
// the stored anchor, 24h back on the first-ever render, and at
// most 7 days back however old the anchor is.
func (app *Application) briefingWindowStart(ctx context.Context, userID int64, now time.Time) time.Time {
	start := now.Add(-briefingFirstWindow)
	raw, err := app.queries.GetUserBriefingAnchor(ctx, userID)
	if err != nil {
		logging.Errorf("home: briefing anchor for user %d: %v", userID, err)
		return start
	}
	if anchor, ok := parseRFC3339(raw); ok && anchor.Before(now) {
		start = anchor
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
// full briefingAnchorStep behind this render (or unreadable,
// which includes first-run). Only a normal home render that
// included the module calls this.
func (app *Application) advanceBriefingAnchor(ctx context.Context, userID int64, now time.Time) {
	raw, err := app.queries.GetUserBriefingAnchor(ctx, userID)
	if err != nil {
		logging.Errorf("home: briefing anchor for user %d: %v", userID, err)
		return
	}
	if anchor, ok := parseRFC3339(raw); ok && !anchor.Before(now.Add(-briefingAnchorStep)) {
		// The stored anchor already sits within a step of this
		// render (or ahead of it, under clock skew): advancing
		// it would change nothing the digest can show, so skip
		// the write.
		return
	}
	if err := app.queries.SetUserBriefingAnchor(ctx, db.SetUserBriefingAnchorParams{
		LastBriefingAt: now.UTC().Format(time.RFC3339),
		ID:             userID,
	}); err != nil {
		logging.Errorf("home: advance briefing anchor for user %d: %v", userID, err)
	}
}

func (app *Application) buildBriefing(ctx context.Context, bundles []*charSnaps, since, now time.Time) *briefingWidget {
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
	charPrefix := func(b *charSnaps) template.HTML {
		return charLink(viewer, b.ch.CharacterID, b.ch.Name)
	}

	var lines []briefingLine
	seen := map[string]bool{}
	add := func(key string, rank int, at time.Time, html template.HTML) {
		if seen[key] {
			return
		}
		seen[key] = true
		lines = append(lines, briefingLine{html: string(html), Rank: rank, At: at, key: key})
	}

	// Industry slot bases need the four slot skills' type IDs,
	// resolved from the local static data by name (type IDs are
	// CCP's to change; names are how the skill catalog speaks).
	slotSkillIDs := app.slotSkillIDs(ctx)

	for _, b := range bundles {
		prefix := string(charPrefix(b))

		// --- Standing: the link itself. -------------------------
		if b.ch.LinkState != "" && b.ch.LinkState != linkStateOK {
			reason := "the saved sign-in stopped working"
			if b.ch.LinkState == linkStateOwnerChanged {
				reason = "the character changed accounts"
			}
			add(briefingKey(b.ch.CharacterID, "relink"), briefingRelink, time.Time{},
				template.HTML(fmt.Sprintf("%s needs re-linking — %s. <a href=\"/characters/\">Characters</a>", prefix, reason)))
		} else if app.piNotEnabled(ctx, b.ch) {
			// A healthy link whose login predates the planetary
			// scope: colonies stay dark until a fresh sign-in.
			add(briefingKey(b.ch.CharacterID, "pi-scope"), briefingRelink, time.Time{},
				template.HTML(fmt.Sprintf("%s — planetary industry stays off until they sign in again. <a href=\"/planets/?character=%d\">Planets</a>", prefix, b.ch.CharacterID)))
		}

		// --- Skills: nothing training, or the queue ends soon. --
		if b.queueKnown {
			training, finishedAt := queueState(b.queue, now)
			switch {
			case !training:
				add(briefingKey(b.ch.CharacterID, "not-training"), briefingNotTraining, finishedAt,
					template.HTML(fmt.Sprintf("%s isn't training — the skill queue is empty. <a href=\"/character/?character=%d\">Skills</a>", prefix, b.ch.CharacterID)))
			default:
				if head := queueHead(b.queue); head != nil {
					if finish, ok := parseRFC3339(lastQueueFinish(b.queue)); ok && finish.After(now) && finish.Before(now.Add(briefingAhead)) {
						skillID := head.SkillID
						skillName := app.typeNameOrID(ctx, skillID)
						add(briefingKey(b.ch.CharacterID, "queue-ending"), briefingSkillEnding, finish,
							template.HTML(fmt.Sprintf("%s finishes %s %s in %s — queue runs dry after that. <a href=\"/character/?character=%d\">Skills</a>",
								prefix, string(itemLink(skillID, skillName)), esi.RomanLevel(head.FinishedLevel),
								humanDuration(time.Until(finish)), b.ch.CharacterID)))
					}
				}
			}
		}

		// --- Industry: finished jobs waiting, idle slots. -------
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
				line += template.HTML(fmt.Sprintf(". <a href=\"/industry/?character=%d\">Industry</a>", b.ch.CharacterID))
				add(briefingKey(b.ch.CharacterID, "job-ready"), briefingJobReady, readyAt, line)
			}

			if b.skillsKnown && len(b.jobs) > 0 {
				levels := briefingSkillLevels(b.skills)
				manufMax := 1 + levels[slotSkillIDs["Mass Production"]] + levels[slotSkillIDs["Advanced Mass Production"]]
				researchMax := 1 + levels[slotSkillIDs["Laboratory Operation"]] + levels[slotSkillIDs["Advanced Laboratory Operation"]]
				if idle := manufMax - activeManuf; idle > 0 {
					add(briefingKey(b.ch.CharacterID, "idle-manufacturing"), briefingIdleSlots, time.Time{},
						template.HTML(fmt.Sprintf("%s has %d of %d manufacturing slots idle. <a href=\"/industry/?character=%d\">Industry</a>", prefix, idle, manufMax, b.ch.CharacterID)))
				}
				if idle := researchMax - activeResearch; idle > 0 {
					add(briefingKey(b.ch.CharacterID, "idle-research"), briefingIdleSlots, time.Time{},
						template.HTML(fmt.Sprintf("%s has %d of %d research slots idle. <a href=\"/industry/?character=%d\">Industry</a>", prefix, idle, researchMax, b.ch.CharacterID)))
				}
			}
		}

		// --- Market: orders expired inside the window, orders
		// expiring within a day. --------------------------------
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
				line += template.HTML(fmt.Sprintf(". <a href=\"/orders/?character=%d\">Orders</a>", b.ch.CharacterID))
				add(briefingKey(b.ch.CharacterID, "order-expired"), briefingOrderExpired, expiredAt, line)
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
				line += template.HTML(fmt.Sprintf(". <a href=\"/orders/?character=%d\">Orders</a>", b.ch.CharacterID))
				add(briefingKey(b.ch.CharacterID, "order-expiring"), briefingOrderExpiring, expiringAt, line)
			}
		}

		// --- Planetary industry: stopped extractors, dry soon. --
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
				add(briefingKey(b.ch.CharacterID, "pi-stopped"), briefingStoppedExtractor, time.Time{},
					template.HTML(fmt.Sprintf("%s — %d extractor%s have stopped across %d planet%s. <a href=\"/planets/?character=%d\">Colonies</a>",
						prefix, stoppedN, pluralSuffix(stoppedN), stoppedPlanets, pluralSuffix(stoppedPlanets), b.ch.CharacterID)))
			}
			if dryN > 0 {
				add(briefingKey(b.ch.CharacterID, "pi-dry"), briefingExtractorDry, dryAt,
					template.HTML(fmt.Sprintf("%s — an extractor on %s runs dry in %s. <a href=\"/planets/?character=%d\">Colonies</a>",
						prefix, template.HTMLEscapeString(dryPlanet), humanDuration(time.Until(dryAt)), b.ch.CharacterID)))
			}
		}

		// --- Contracts: waiting answers, courier deadlines. -----
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
					add(briefingKey(b.ch.CharacterID, fmt.Sprintf("courier-%d", c.ContractID)), briefingCourier, deadline,
						template.HTML(fmt.Sprintf("%s — courier: %s is %s. <a href=\"/contracts/?character=%d\">Contracts</a>",
							prefix, template.HTMLEscapeString(title), when, b.ch.CharacterID)))
				}
			}
			if waitingN > 0 {
				add(briefingKey(b.ch.CharacterID, "contract-waiting"), briefingContract, waitingAt,
					template.HTML(fmt.Sprintf("%s — %d contract%s waiting for your answer. <a href=\"/contracts/?character=%d\">Contracts</a>",
						prefix, waitingN, pluralSuffix(waitingN), b.ch.CharacterID)))
			}
		}

		// --- Calendar: events inside the next day. ---------------
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
				line := template.HTML(fmt.Sprintf("%s — “%s” starts in %s. <a href=\"/calendar/?character=%d\">Calendar</a>",
					prefix, template.HTMLEscapeString(title), humanDuration(time.Until(at)), b.ch.CharacterID))
				if at.Before(now) {
					line = template.HTML(fmt.Sprintf("%s — “%s” has started. <a href=\"/calendar/?character=%d\">Calendar</a>",
						prefix, template.HTMLEscapeString(title), b.ch.CharacterID))
				}
				add(briefingKey(b.ch.CharacterID, fmt.Sprintf("calendar-%d", ev.EventID)), briefingCalendar, at, line)
			}
		}

		// --- Mail: unread count + newest sender. -----------------
		if b.mailLabels != nil && b.mailLabels.TotalUnreadCount > 0 {
			line := template.HTML(fmt.Sprintf("%s has %d unread mail%s. <a href=\"/mail/?character=%d\">Mail</a>",
				prefix, b.mailLabels.TotalUnreadCount, pluralSuffix(int(b.mailLabels.TotalUnreadCount)), b.ch.CharacterID))
			if b.mailKnown {
				if hdr, ok := newestUnreadHeader(b.mail); ok {
					if sender, found := app.esi.CachedCharacterName(hdr.From); found && sender != "" {
						line = template.HTML(fmt.Sprintf("%s has %d unread mail%s — newest from %s. <a href=\"/mail/?character=%d\">Mail</a>",
							prefix, b.mailLabels.TotalUnreadCount, pluralSuffix(int(b.mailLabels.TotalUnreadCount)),
							string(charLink(viewer, hdr.From, sender)), b.ch.CharacterID))
					}
				}
			}
			add(briefingKey(b.ch.CharacterID, "mail"), briefingMail, time.Time{}, line)
		}
	}

	// --- Account-level market lines: undercut sell orders and
	// watched-price moves, from the worker's stored verdicts and
	// price history (the Needs-attention feed's own sources). ----
	lines = append(lines, app.briefingMarketLines(ctx, bundles, viewer, names)...)

	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].Rank != lines[j].Rank {
			return lines[i].Rank < lines[j].Rank
		}
		return lines[i].At.Before(lines[j].At)
	})
	if len(lines) > briefingCap {
		w.More = len(lines) - briefingCap
		lines = lines[:briefingCap]
	}
	for _, l := range lines {
		w.Lines = append(w.Lines, l)
		w.HTMLLines = append(w.HTMLLines, template.HTML(l.html))
	}
	return w
}

// briefingMarketLines builds the account-level market lines:
// undercut sell orders (one line per character, their worst
// example named) and watchlist moves past the saved threshold
// (one per watched item), from stored verdicts and history.
func (app *Application) briefingMarketLines(ctx context.Context, bundles []*charSnaps, viewer map[int64]bool, names map[int64]string) []briefingLine {
	if len(bundles) == 0 {
		return nil
	}
	userID := bundles[0].ch.UserID
	var lines []briefingLine

	health, err := app.queries.ListOrderHealthByUser(ctx, userID)
	if err != nil {
		logging.Errorf("home: briefing: list order health for user %d: %v", userID, err)
	} else {
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
			line := template.HTML(fmt.Sprintf("%s has %d sell order%s undercut right now — %s is no longer the best price. <a href=\"/market/\">Market</a>",
				string(charLink(viewer, id, names[id])),
				n, pluralSuffix(n),
				string(itemLink(ex.TypeID, app.typeNameOrID(ctx, ex.TypeID)))))
			at, _ := parseRFC3339(ex.ComputedAt)
			lines = append(lines, briefingLine{html: string(line), Rank: briefingUndercut, At: at})
		}
	}

	entries, err := app.queries.ListWatchlistByUser(ctx, userID)
	if err != nil {
		logging.Errorf("home: briefing: list watchlist for user %d: %v", userID, err)
		return lines
	}
	for _, e := range entries {
		rows := app.recentHistoryRows(ctx, e.RegionID, e.TypeID, historyChartRows)
		pct, ok := historyChangePct(rows, 7)
		if !ok || absFloat(pct) < e.ThresholdPct {
			continue
		}
		line := template.HTML(fmt.Sprintf("%s moved %s over 7 days in %s — past your %s watch. <a href=\"/market/\">Watchlist</a>",
			string(itemLink(e.TypeID, app.typeNameOrID(ctx, e.TypeID))),
			changeDirection(pct),
			template.HTMLEscapeString(app.marketRegionLabel(ctx, e.RegionID)),
			formatChangePct(e.ThresholdPct)))
		lines = append(lines, briefingLine{html: string(line), Rank: briefingWatchMove})
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
func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// pluralVerb: "s" for one, "" otherwise ("1 order expires").
func pluralVerb(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}
