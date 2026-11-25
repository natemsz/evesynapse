package app

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Corp Pulse's kinds of change, as its kinds option names them.
const (
	pulseKindJoined   = "joined"
	pulseKindLeft     = "left"
	pulseKindWallet   = "wallet"
	pulseKindAnchored = "anchored"
	pulseKindRemoved  = "removed"
	pulseKindTimers   = "timers"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetPulse, Title: "Corp Pulse", Category: wcatCorp, Feature: "corp_members",
		Requires:    "corpdata",
		Href:        "/corporations/",
		Description: "What changed in the corporation: members in and out, wallet movements, structure arrivals and timers. Needs a qualifying in-game role.",
		Default:     wSize{1, 1},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "How many of each change, or a quiet note."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "The counts and the two latest changes."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "Every change in one list, newest first."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The list of changes beside the largest wallet moves."},
			{Name: "large", Sizes: "3x2 3x3", Says: "A column for each corporation."},
		},
		Options: []widgetOption{
			optCorp(),
			optMulti("kinds", "Show", strings.Join([]string{pulseKindJoined, pulseKindLeft, pulseKindWallet, pulseKindAnchored, pulseKindRemoved, pulseKindTimers}, ","), 0,
				pulseKindJoined+":Members who joined", pulseKindLeft+":Members who left", pulseKindWallet+":Wallet moves",
				pulseKindAnchored+":Structures anchored", pulseKindRemoved+":Structures removed", pulseKindTimers+":Structure timers"),
		},
		Build: buildPulseWidget,
	})
}

// pulseEvent is one change as the feed lists it.
type pulseEvent struct {
	Kind string // one of the pulseKind* values, for its mark
	Says string // "Joined", "Left", "Wallet", ...
	Text string
	When string
	Corp string
	at   time.Time
}

// pulseCount is one kind of change and how many there were.
type pulseCount struct {
	Kind, Says string
	N          int
}

// pulseCorpFeed is one corporation's changes, newest first.
type pulseCorpFeed struct {
	Name   string
	Counts []pulseCount
	Feed   []pulseEvent
}

// pulseView is what the Corp Pulse forms draw.
type pulseView struct {
	Counts []pulseCount    // the kinds with changes, across the corporations shown
	Feed   []pulseEvent    // every change shown, newest first (timers by their end)
	Moves  []pulseEvent    // medium: the largest wallet moves
	Corps  []pulseCorpFeed // large: a column per corporation
	Empty  wEmpty
}

// pulseFeedMax bounds the feed; the card shows the rows that fit.
const pulseFeedMax = 20

func buildPulseWidget(ctx context.Context, app *Application, in *widgetInput) any {
	raw := app.buildPulse(ctx, in.UserID)
	v := &pulseView{}
	corps := raw.Corps
	if id := corpOptionID(in.Opts.Get("corp")); id > 0 {
		var kept []pulseCorpView
		for _, c := range corps {
			if c.CorpID == id {
				kept = append(kept, c)
			}
		}
		// A corporation the card can no longer read reads as all.
		if len(kept) > 0 {
			corps = kept
		}
	}
	if len(corps) == 0 {
		v.Empty = wEmpty{Text: "Nothing to show. This needs a Director, Personnel Manager, Accountant, or Station Manager role in the corporation."}
		return v
	}
	kinds := map[string]bool{}
	for _, k := range strings.Split(in.Opts.Get("kinds"), ",") {
		kinds[k] = true
	}
	total := map[string]int{}
	for _, c := range corps {
		feed := pulseCorpEvents(c, kinds)
		cf := pulseCorpFeed{Name: c.Name, Feed: feed, Counts: pulseCounts(feed)}
		for _, e := range feed {
			total[e.Kind]++
		}
		v.Feed = append(v.Feed, feed...)
		if in.Draws("large") {
			v.Corps = append(v.Corps, cf)
		}
		if in.Draws("medium") && kinds[pulseKindWallet] {
			for _, m := range c.Moves {
				v.Moves = append(v.Moves, pulseEvent{Kind: pulseKindWallet, Says: "Division " + strconv.FormatInt(m.Division, 10), Text: m.Text, When: m.When, Corp: c.Name, at: m.At})
			}
		}
	}
	sort.SliceStable(v.Feed, func(i, j int) bool { return v.Feed[i].at.After(v.Feed[j].at) })
	if len(v.Feed) > pulseFeedMax {
		v.Feed = v.Feed[:pulseFeedMax]
	}
	for _, k := range pulseKindOrder {
		if total[k.Kind] > 0 {
			v.Counts = append(v.Counts, pulseCount{Kind: k.Kind, Says: k.Says, N: total[k.Kind]})
		}
	}
	if len(corps) < 2 {
		for i := range v.Feed {
			v.Feed[i].Corp = ""
		}
		for i := range v.Moves {
			v.Moves[i].Corp = ""
		}
	}
	return v
}

// pulseKindOrder is the order the counts read in, and each kind's word.
var pulseKindOrder = []pulseCount{
	{Kind: pulseKindJoined, Says: "Joined"}, {Kind: pulseKindLeft, Says: "Left"}, {Kind: pulseKindWallet, Says: "Wallet"},
	{Kind: pulseKindAnchored, Says: "Anchored"}, {Kind: pulseKindRemoved, Says: "Removed"}, {Kind: pulseKindTimers, Says: "Timer"},
}

// pulseCorpEvents is one corporation's changes of the kinds asked
// for, newest first; a timer sorts by when it ends.
func pulseCorpEvents(c pulseCorpView, kinds map[string]bool) []pulseEvent {
	var out []pulseEvent
	add := func(kind, text, when string, at time.Time) {
		if kinds[kind] {
			out = append(out, pulseEvent{Kind: kind, Says: pulseKindSays(kind), Text: text, When: when, Corp: c.Name, at: at})
		}
	}
	for _, j := range c.Joined {
		add(pulseKindJoined, j.Name, j.Joined, j.At)
	}
	for _, l := range c.Left {
		add(pulseKindLeft, l.Text, l.When, l.At)
	}
	for _, m := range c.Moves {
		add(pulseKindWallet, "Division "+strconv.FormatInt(m.Division, 10)+": "+m.Text, m.When, m.At)
	}
	for _, a := range c.Added {
		add(pulseKindAnchored, a.Text, a.When, a.At)
	}
	for _, r := range c.Removed {
		add(pulseKindRemoved, r.Text, r.When, r.At)
	}
	for _, t := range c.Timers {
		add(pulseKindTimers, t.Name+": "+t.Text, t.When, t.At)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	return out
}

func pulseKindSays(kind string) string {
	for _, k := range pulseKindOrder {
		if k.Kind == kind {
			return k.Says
		}
	}
	return kind
}

// pulseCounts is how many changes of each kind a feed holds, the
// kinds with none left out.
func pulseCounts(feed []pulseEvent) []pulseCount {
	n := map[string]int{}
	for _, e := range feed {
		n[e.Kind]++
	}
	var out []pulseCount
	for _, k := range pulseKindOrder {
		if n[k.Kind] > 0 {
			out = append(out, pulseCount{Kind: k.Kind, Says: k.Says, N: n[k.Kind]})
		}
	}
	return out
}
