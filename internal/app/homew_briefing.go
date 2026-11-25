package app

import (
	"context"
	"fmt"
	"html/template"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetBriefing, Title: "Briefing", Category: wcatOverview, Feature: "briefing",
		Description: "What changed since you last looked, and what needs you in the next day.",
		Default:     wSize{3, 1},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "How many alerts of each severity, and the most urgent one."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "One row of small alert cards by kind, with arrows for the rest."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "Every alert as a line, grouped by kind."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "Alert cards two across, each flipping through its alerts."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Alert cards three across, with severity filters and the dismissed list."},
		},
		Needs: []string{esi.SnapSkillqueue, esi.SnapSkills, esi.SnapIndustryJobs, esi.SnapContracts, esi.SnapOrders, esi.SnapOrdersHistory, esi.SnapPlanets, esi.SnapMail, esi.SnapMailLabels, esi.SnapCalendar, needPlanetLayouts},
		Options: []widgetOption{
			optChoice("show", "Show", briefShowAll,
				briefShowAll+":Everything", briefShowWarning+":Warnings and critical", briefShowCritical+":Critical only"),
			withHint(optMulti("topics", "Topics", strings.Join(briefingTopicValues(), ","), 0, briefingTopicChoices()...),
				"Alerts about the topics you leave unticked stay off the Briefing."),
		},
		Build: buildBriefingWidget,
	})
}

// The show option's values: which severities the Briefing lists.
const (
	briefShowAll      = "all"
	briefShowWarning  = "warning"
	briefShowCritical = "critical"
)

// briefingTopics are the kinds of alert the topics option picks
// from, by kind and in the words its settings sheet uses. A topic
// whose module is not built in never has an alert to show.
var briefingTopics = []struct{ Kind, Label string }{
	{"skills", "Skill training"},
	{"industry", "Industry jobs"},
	{"pi", "Planetary industry"},
	{"orders", "Market orders"},
	{"market", "Watched prices"},
	{"contract", "Contracts"},
	{"calendar", "Calendar events"},
	{"mail", "Unread mail"},
	{"srp", "Ship replacement claims to review"},
	{"community", "Community join requests"},
	{"camp", "Gate camps"},
	{"link", "Characters to link again"},
}

func briefingTopicValues() []string {
	out := make([]string, len(briefingTopics))
	for i, t := range briefingTopics {
		out[i] = t.Kind
	}
	return out
}

func briefingTopicChoices() []string {
	out := make([]string, len(briefingTopics))
	for i, t := range briefingTopics {
		out[i] = t.Kind + ":" + t.Label
	}
	return out
}

// briefingFilter is what the widget's options let through: the
// severities shown and the topics picked (nil picks every topic).
type briefingFilter struct {
	show   string
	topics map[string]bool
}

// briefingFilterFrom reads the widget's options.
func briefingFilterFrom(opts widgetOptions) briefingFilter {
	f := briefingFilter{show: opts.Get("show")}
	if v := opts.Get("topics"); v != "" {
		picks := strings.Split(v, ",")
		if len(picks) < len(briefingTopics) {
			f.topics = make(map[string]bool, len(picks))
			for _, p := range picks {
				f.topics[p] = true
			}
		}
	}
	return f
}

// wants reports whether alerts of a kind are shown at all.
func (f briefingFilter) wants(kind string) bool { return f.topics == nil || f.topics[kind] }

// keeps reports whether a line of this rule is shown.
func (f briefingFilter) keeps(rank int) bool {
	if !f.wants(briefingKind(rank)) {
		return false
	}
	switch f.show {
	case briefShowCritical:
		return briefingSeverity(rank) == "critical"
	case briefShowWarning:
		return briefingSeverity(rank) != "info"
	}
	return true
}

// briefingView is what the Briefing's forms draw: the digest, the
// severity counts as pills, and its most urgent alert.
type briefingView struct {
	*briefingWidget
	Pills   []briefingPill
	Top     *briefingItem // small: the first of the worst severity
	TopKind string        // the name of Top's kind
	Empty   wEmpty
}

// briefingPill is one severity and how many alerts carry it.
type briefingPill struct {
	Sev, Label string
	N          int
}

func buildBriefingWidget(ctx context.Context, app *Application, in *widgetInput) any {
	b := app.buildBriefing(ctx, in.Bundles, in.Shared.BriefingSince(ctx), in.Now, in.Shared.market, briefingFilterFrom(in.Opts))
	v := &briefingView{briefingWidget: b, Empty: wEmpty{Text: "Nothing needs your attention right now."}}
	for _, p := range []briefingPill{{"critical", "Critical", b.Counts.Critical}, {"warning", "Warning", b.Counts.Warning}, {"info", "Info", b.Counts.Info}} {
		if p.N > 0 {
			v.Pills = append(v.Pills, p)
		}
	}
	for i := range b.Items {
		if v.Top == nil || briefingSeverityRank(b.Items[i].Severity) < briefingSeverityRank(v.Top.Severity) {
			v.Top = &b.Items[i]
		}
	}
	if v.Top != nil {
		v.TopKind = briefingKindTitle(v.Top.Kind)
	}
	return v
}

// briefingOwnersCap bounds the owners whose waiting claims are
// counted: one count per owner, each a single indexed read.
const briefingOwnersCap = 12

// briefingWaitingLines: what other people wait on the account for.
// For each corporation, alliance or community whose ship replacement
// claims the account may answer, the claims waiting for review; for
// each community the account leads or helps run, the pilots asking to
// join. Bounded reads: the account's corporations, its communities
// (one query, capped), one count per owner (capped), and the account's
// standing only when a claim waits. The lines are built whatever the
// Briefing's topics say: the topic filter hides them after the
// dismissal bookkeeping, so a dismissal survives its topic being
// switched off and on.
func (app *Application) briefingWaitingLines(ctx context.Context, userID int64, bundles []*charSnaps) []briefingLine {
	wantSRP := moduleCompiledIn("srp")
	wantJoin := moduleCompiledIn("communities")
	if userID == 0 || len(bundles) == 0 || (!wantSRP && !wantJoin) {
		return nil
	}
	ids := make([]int64, 0, len(bundles))
	for _, b := range bundles {
		ids = append(ids, b.ch.CharacterID)
	}
	communities, err := app.queries.ListCommunitiesForCharacters(ctx, ids)
	if err != nil {
		logging.Errorf("home: communities of user %d: %v", userID, err)
	}
	var lines []briefingLine
	if wantJoin {
		for _, c := range communities {
			if c.State != "active" || c.Role == standing.CommunityMember || c.PendingCount <= 0 || c.ArchivedAt.Valid {
				continue
			}
			n := int(c.PendingCount)
			href := "/fleets/communities/" + c.Slug + "/"
			lines = append(lines, briefingLine{
				html: fmt.Sprintf(`%d pilot%s ask%s to join <a href="%s">%s</a>.`, n, pluralSuffix(n), pluralVerb(n), href, template.HTMLEscapeString(c.Name)),
				Rank: briefingJoinRequests, key: fmt.Sprintf("join|%d", c.ID), href: href,
			})
		}
	}
	if wantSRP {
		lines = append(lines, app.briefingSRPLines(ctx, userID, communities)...)
	}
	return lines
}

// briefingSRPLines: the ship replacement claims waiting for review at
// each owner the account may answer for. The owners are found the way
// the ship replacement notices find them: the corporations of the
// account's characters, their alliances, and the communities they are
// active in.
func (app *Application) briefingSRPLines(ctx context.Context, userID int64, communities []db.ListCommunitiesForCharactersRow) []briefingLine {
	rows, err := app.queries.ListWorkingCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("home: corporations of user %d: %v", userID, err)
	}
	seen := map[orgOwner]bool{}
	var owners []orgOwner
	add := func(o orgOwner) {
		if o.ID != 0 && !seen[o] && len(owners) < briefingOwnersCap {
			seen[o] = true
			owners = append(owners, o)
		}
	}
	for _, row := range rows {
		if row.CorporationID == 0 {
			continue
		}
		add(orgOwner{Kind: ownerCorporation, ID: row.CorporationID})
		if alliance, _ := app.corpAlliance(ctx, row.CorporationID); alliance != 0 {
			add(orgOwner{Kind: ownerAlliance, ID: alliance})
		}
	}
	for _, c := range communities {
		if c.State == "active" {
			add(orgOwner{Kind: ownerCommunity, ID: c.ID})
		}
	}
	var st orgStanding
	var stOK, stTried bool
	var lines []briefingLine
	for _, o := range owners {
		open, err := app.queries.CountOpenSRPRequests(ctx, db.CountOpenSRPRequestsParams{OwnerKind: o.Kind, OwnerID: o.ID})
		if err != nil || open.Requests == 0 {
			continue
		}
		if !stTried {
			stTried = true
			st, err = app.standingFor(ctx, userID)
			stOK = err == nil
		}
		if !stOK || !app.ownerPermits(ctx, st, o, permSRP) {
			continue
		}
		n := int(open.Requests)
		href := standing.Address(srpPath, o.Key())
		html := fmt.Sprintf(`%d ship replacement claim%s wait%s for review in <a href="%s">%s</a>`, n, pluralSuffix(n), pluralVerb(n), href, template.HTMLEscapeString(app.ownerName(ctx, o)))
		if open.Owed > 0 {
			html += ", " + iskCompact(open.Owed) + " ISK suggested"
		}
		lines = append(lines, briefingLine{html: html + ".", Rank: briefingSRPReview, key: "srp|" + o.Key(), href: href})
	}
	return lines
}
