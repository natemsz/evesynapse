package app

import (
	"context"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// Facts several home widgets show, each computed once per render on the
// render's cache (homeShared): Coming Up lists the same fleets, extractor
// stops and structure deadlines that Fleets, Planetary Industry and
// Upkeep & Fuel draw. Every getter reads the whole account; a widget
// with a scope narrows the rows to its characters (forChars).
// ---------------------------------------------------------------------------

// forChars is the rows that belong to the given characters, in a new
// slice (the cached rows are never changed in place).
func forChars[T any](rows []T, chars []db.Character, charOf func(T) int64) []T {
	keep := make(map[int64]bool, len(chars))
	for _, c := range chars {
		keep[c.CharacterID] = true
	}
	out := make([]T, 0, len(rows))
	for _, r := range rows {
		if keep[charOf(r)] {
			out = append(out, r)
		}
	}
	return out
}

// PIExpiries is every extractor stop across the account's pilots,
// soonest first.
func (s *homeShared) PIExpiries(ctx context.Context) []piExpiryLine {
	if !s.piRowsBuilt {
		s.piRows, s.piRowsBuilt = piExpiryRows(ctx, s.app, s.bundles, s.now), true
	}
	return s.piRows
}

// Upkeep is the account's structure deadlines within the week, for the
// corporations where a character holds a role that may read them.
func (s *homeShared) Upkeep(ctx context.Context) *upkeepWidget {
	if s.upkeep == nil {
		s.upkeep = s.app.buildUpkeep(ctx, s.userID)
	}
	return s.upkeep
}

// homeFleet is one fleet the account can see, with what the account
// may do on it and its own best answer.
type homeFleet struct {
	Op     db.Op
	Access fleetAccess
	Answer string // opYes, opMaybe, opNo or ""
	Owner  string // the owning corporation, alliance or community
	Live   bool   // started and not cancelled
}

// homeFleetsAhead is how far ahead the home reads fleets: the longest
// window Coming Up offers.
const homeFleetsAhead = 7 * 24 * time.Hour

// Fleets is every fleet the account can open on the Fleets page that is
// running or starts within homeFleetsAhead, earliest first, cancelled
// ones left out. It is the Fleets page's own read (visibleFleets), so
// the home never shows a fleet that page would hide.
func (s *homeShared) Fleets(ctx context.Context) []homeFleet {
	if s.fleetsBuilt {
		return s.fleets
	}
	s.fleetsBuilt = true
	if s.userID == 0 {
		return nil
	}
	v := s.app.accountFleetViewer(ctx, s.userID)
	ops, facts, _ := s.app.visibleFleets(ctx, v, s.now, s.now.Add(homeFleetsAhead), true)
	answers := s.app.fleetAnswers(ctx, v, ops)
	names := map[string]string{}
	for _, op := range ops {
		if op.CancelledAt.Valid {
			continue
		}
		owner := opOwner(op)
		name, known := names[owner.Key()]
		if !known {
			name = s.app.ownerName(ctx, owner)
			names[owner.Key()] = name
		}
		s.fleets = append(s.fleets, homeFleet{
			Op: op, Access: v.access(op, facts[op.ID]), Answer: answers[op.ID], Owner: name,
			Live: !op.StartsAt.After(s.now),
		})
	}
	sortFleetsByStart(s.fleets)
	return s.fleets
}

// IntelChannels is the account's intel identity and the channels it
// can read, the intel feed's own access check (accessibleChannels), so
// the home never shows a report the feed would hide.
func (s *homeShared) IntelChannels(ctx context.Context) (intelIdentity, []accessChannel) {
	if !s.intelChansBuilt {
		s.intelChansBuilt = true
		if s.userID != 0 {
			s.intelID = s.app.intelIdentityFor(ctx, s.userID, 0)
			s.intelChannels = s.app.accessibleChannels(ctx, s.intelID)
		}
	}
	return s.intelID, s.intelChannels
}

// sortFleetsByStart orders fleets by start, running ones first.
func sortFleetsByStart(fleets []homeFleet) {
	sort.SliceStable(fleets, func(i, j int) bool { return fleets[i].Op.StartsAt.Before(fleets[j].Op.StartsAt) })
}
