package app

import (
	"context"
	"sort"
	"strings"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetFleet, Title: "Fleet Overview", Category: wcatPilots, Feature: "fleet",
		Href:        "/characters/",
		Description: "Every linked character at a glance: where they are, what they're flying, what they're training.",
		Default:     wSize{3, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "How many pilots are online, training and idle."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "Portraits with online dots, as many as fit."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "Each pilot's system and training, with a filter on taller cards."},
			{Name: "medium", Sizes: "2x2 2x3 2x4", Says: "The pilot table with location, training and wallet."},
			{Name: "large", Sizes: "3x2 3x3 3x4", Says: "The full table with ships and each pilot's details."},
		},
		Needs: []string{esi.SnapProfile, esi.SnapCorpInfo, esi.SnapLocation, esi.SnapShip, esi.SnapOnline, esi.SnapSkillqueue, esi.SnapWallet, esi.SnapMailLabels},
		Options: []widgetOption{
			optScope(),
			optChoice("sort", "Sort by", "name", "name:Name", "training:Training ends soonest", "wallet:Wallet, largest first", "location:Location"),
			withHint(optToggle("idle", "Idle pilots first", false), "Pilots with nothing training lead the list."),
		},
		Build: buildFleetWidget,
	})
}

// fleetView is what the Fleet Overview forms draw: the rows the scope
// keeps, in the order the sort option says, and the counts the small
// form leads with.
type fleetView struct {
	*fleetWidget
	Online      int
	OnlineKnown bool
	Training    int
	Empty       wEmpty
}

func buildFleetWidget(ctx context.Context, app *Application, in *widgetInput) any {
	// The rollup is the whole account's, cached per user; the card
	// keeps the rows its scope covers.
	all := app.buildFleet(ctx, in.UserID, in.Shared.bundles)
	keep := make(map[int64]bool, len(in.Chars))
	wallet := make(map[int64]float64, len(in.Bundles))
	for _, c := range in.Chars {
		keep[c.CharacterID] = true
	}
	for _, b := range in.Bundles {
		if b.walletKnown {
			wallet[b.ch.CharacterID] = b.wallet
		}
	}
	w := &fleetWidget{}
	tagSeen := map[string]bool{}
	var isk float64
	for _, r := range all.Rows {
		if !keep[r.ID] {
			continue
		}
		w.Rows = append(w.Rows, r)
		for _, tag := range splitTags(r.Tags) {
			tagSeen[tag] = true
		}
		if r.QueueKnown && r.NotTraining {
			w.Idle++
		}
		isk += wallet[r.ID]
	}
	for tag := range tagSeen {
		w.Tags = append(w.Tags, tag)
	}
	sort.Strings(w.Tags)
	if len(wallet) > 0 {
		w.TotalISK = esi.FormatISK(isk)
	}
	sortFleetRows(w.Rows, in.Opts.Get("sort"), in.Opts.Get("idle") == toggleOn, wallet)

	v := &fleetView{fleetWidget: w, Empty: wEmpty{Text: "No characters in this view.", Step: "Link a character", Href: "/characters/"}}
	for _, r := range w.Rows {
		if r.OnlineKnown {
			v.OnlineKnown = true
			if r.Online {
				v.Online++
			}
		}
		if r.QueueKnown && !r.NotTraining {
			v.Training++
		}
	}
	return v
}

// sortFleetRows orders the pilots by the sort option: by name, by the
// soonest training finish (idle pilots after), by wallet, or by
// system. With idle first, pilots training nothing lead whatever the
// order.
func sortFleetRows(rows []fleetRow, by string, idleFirst bool, wallet map[int64]float64) {
	idle := func(r fleetRow) bool { return r.QueueKnown && r.NotTraining }
	less := func(a, b fleetRow) bool { return strings.ToLower(a.Name) < strings.ToLower(b.Name) }
	switch by {
	case "training":
		ends := func(r fleetRow) string {
			if r.NotTraining {
				return ""
			}
			return r.TrainingFinish
		}
		less = func(a, b fleetRow) bool {
			if (ends(a) == "") != (ends(b) == "") {
				return ends(a) != ""
			}
			return ends(a) < ends(b)
		}
	case "wallet":
		less = func(a, b fleetRow) bool { return wallet[a.ID] > wallet[b.ID] }
	case "location":
		less = func(a, b fleetRow) bool {
			if (a.SystemName == "") != (b.SystemName == "") {
				return a.SystemName != ""
			}
			return a.SystemName < b.SystemName
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if idleFirst && idle(rows[i]) != idle(rows[j]) {
			return idle(rows[i])
		}
		return less(rows[i], rows[j])
	})
}
