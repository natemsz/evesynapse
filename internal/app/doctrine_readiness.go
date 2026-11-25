package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
)

// Doctrine readiness (schema 183): for every account, which of its
// pilots can fly each fit of the doctrines its corporations, alliances
// and communities keep, and how close the nearest pilot is when one
// cannot. "Can fly" is the fitting page's test (fitview.FitMissingList):
// every skill the hull, its modules, charges and implants ask for,
// prerequisites included, at the pilot's active level. The worker folds
// the rows from the stored skill snapshots; the home's Doctrine
// Readiness widget only reads them.

// readinessMissingShown is how many missing skills a verdict names;
// the rest are counted.
const readinessMissingShown = 3

// readinessRounds is how deep the requirement walk goes: item, skill,
// prerequisite and on. Five covers the deepest chain in the game.
const readinessRounds = 6

// readinessFit is one doctrine fit the fold judges: every type it
// carries.
type readinessFit struct {
	ID, DoctrineID int64
	Types          []int64
}

// readinessPilot is one pilot's side of the fold: active levels and the
// skill points already in each skill.
type readinessPilot struct {
	ID     int64
	Levels map[int64]int
	SP     map[int64]int64
}

// readinessGap is what one pilot lacks for one fit.
type readinessGap struct {
	CharacterID, FitID int64
	Missing            string // "Heavy Assault Cruisers IV, Large Projectile Turret III"
	MissingN           int
	SP                 int64 // skill points left to train
}

// fitReadiness is one fit's verdict for an account.
type fitReadiness struct {
	FitID, DoctrineID int64
	Ready             []int64 // pilots who can fly it, by id
	Near              readinessGap
}

// doctrineReadinessRow is one doctrine's verdict for an account.
type doctrineReadinessRow struct {
	DoctrineID                       int64
	Fits, FitsFlyable, Pilots, Ready int
	Near                             readinessGap // for a pilot who can fly none of its fits
}

// fitTypes is every type a fit carries that can ask for a skill: the
// hull, each line, each loaded charge and each implant.
func fitTypes(shipTypeID int64, doc fitview.FitDoc) []int64 {
	seen := map[int64]bool{}
	var out []int64
	add := func(id int64) {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(shipTypeID)
	add(doc.ShipTypeID)
	for _, it := range doc.Items {
		add(it.TypeID)
		for _, c := range it.Charges {
			add(c)
		}
	}
	for _, c := range doc.Charges {
		add(c)
	}
	for _, id := range doc.Implants {
		add(id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// skillNeed is the level each skill must reach for the given types,
// prerequisites included.
func skillNeed(types []int64, reqs map[int64][]db.SdeRequirement) map[int64]int64 {
	need := map[int64]int64{}
	seen := map[int64]bool{}
	queue := append([]int64(nil), types...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, r := range reqs[id] {
			if need[r.SkillTypeID] < r.Level {
				need[r.SkillTypeID] = r.Level
			}
			queue = append(queue, r.SkillTypeID)
		}
	}
	return need
}

// pilotGap is what a pilot lacks against a need; MissingN 0 means the
// pilot can fly it.
func pilotGap(p readinessPilot, need map[int64]int64, rank map[int64]float64, skillName func(int64) string) readinessGap {
	g := readinessGap{CharacterID: p.ID}
	type miss struct {
		name string
		sp   int64
	}
	var misses []miss
	for skill, lvl := range need {
		if int64(p.Levels[skill]) >= lvl {
			continue
		}
		r := rank[skill]
		if r <= 0 {
			r = 1
		}
		left := spAtLevel(r, lvl) - p.SP[skill]
		if left < 0 {
			left = 0
		}
		g.SP += left
		misses = append(misses, miss{name: skillName(skill) + " " + esi.RomanLevel(int(lvl)), sp: left})
	}
	g.MissingN = len(misses)
	// The biggest gaps first: what the pilot would train longest.
	sort.Slice(misses, func(i, j int) bool {
		if misses[i].sp != misses[j].sp {
			return misses[i].sp > misses[j].sp
		}
		return misses[i].name < misses[j].name
	})
	names := make([]string, 0, readinessMissingShown)
	for i, m := range misses {
		if i == readinessMissingShown {
			break
		}
		names = append(names, m.name)
	}
	g.Missing = strings.Join(names, ", ")
	return g
}

// nearer reports whether gap a is closer to ready than b (b unset counts
// as far).
func nearer(a, b readinessGap) bool {
	if b.CharacterID == 0 {
		return true
	}
	if a.SP != b.SP {
		return a.SP < b.SP
	}
	if a.MissingN != b.MissingN {
		return a.MissingN < b.MissingN
	}
	if a.CharacterID != b.CharacterID {
		return a.CharacterID < b.CharacterID
	}
	return a.FitID < b.FitID
}

// foldDoctrineReadiness judges every fit against every pilot. Pilots
// come with their skills known; fits in id order.
func foldDoctrineReadiness(fits []readinessFit, pilots []readinessPilot, reqs map[int64][]db.SdeRequirement,
	rank map[int64]float64, skillName func(int64) string) ([]fitReadiness, []doctrineReadinessRow) {
	byDoctrine := map[int64]*doctrineReadinessRow{}
	var order []int64
	readyFor := map[int64]map[int64]bool{}        // doctrine -> pilots who can fly one of its fits
	gapsFor := map[int64]map[int64]readinessGap{} // doctrine -> pilot -> nearest gap
	out := make([]fitReadiness, 0, len(fits))
	for _, f := range fits {
		d := byDoctrine[f.DoctrineID]
		if d == nil {
			d = &doctrineReadinessRow{DoctrineID: f.DoctrineID, Pilots: len(pilots)}
			byDoctrine[f.DoctrineID] = d
			order = append(order, f.DoctrineID)
			readyFor[f.DoctrineID] = map[int64]bool{}
			gapsFor[f.DoctrineID] = map[int64]readinessGap{}
		}
		d.Fits++
		need := skillNeed(f.Types, reqs)
		fr := fitReadiness{FitID: f.ID, DoctrineID: f.DoctrineID, Ready: []int64{}}
		for _, p := range pilots {
			g := pilotGap(p, need, rank, skillName)
			g.FitID = f.ID
			if g.MissingN == 0 {
				fr.Ready = append(fr.Ready, p.ID)
				readyFor[f.DoctrineID][p.ID] = true
				continue
			}
			if nearer(g, fr.Near) {
				fr.Near = g
			}
			if prev, ok := gapsFor[f.DoctrineID][p.ID]; !ok || nearer(g, prev) {
				gapsFor[f.DoctrineID][p.ID] = g
			}
		}
		if len(fr.Ready) > 0 {
			d.FitsFlyable++
		}
		out = append(out, fr)
	}
	docs := make([]doctrineReadinessRow, 0, len(order))
	for _, id := range order {
		d := byDoctrine[id]
		d.Ready = len(readyFor[id])
		for pid, g := range gapsFor[id] {
			if readyFor[id][pid] {
				continue // flies another of the doctrine's fits
			}
			if nearer(g, d.Near) {
				d.Near = g
			}
		}
		docs = append(docs, *d)
	}
	return out, docs
}

// refreshDoctrineReadiness folds an account's doctrine readiness when
// what it reads has moved: the account's pilots, their skill snapshots,
// the owners whose doctrines it reads, or those doctrines' fits. The
// watermark reads stamps and one fingerprint, so passes that find
// nothing new stay cheap. Best-effort: a failure logs and leaves the
// last verdicts in place.
func (app *Application) refreshDoctrineReadiness(ctx context.Context, userID int64) {
	if !moduleCompiledIn("doctrines") || userID == 0 {
		return
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil || len(chars) == 0 {
		return
	}
	charIDs := make([]int64, len(chars))
	for i, c := range chars {
		charIDs[i] = c.CharacterID
	}
	sort.Slice(charIDs, func(i, j int) bool { return charIDs[i] < charIDs[j] })
	owners := app.DoctrineOwners(ctx, userID)
	keys := make([]string, 0, len(owners))
	for _, o := range owners {
		keys = append(keys, o.Kind+":"+strconv.FormatInt(o.ID, 10))
	}
	sort.Strings(keys)
	fp, err := app.queries.DoctrineFitsFingerprint(ctx, keys)
	if err != nil {
		logging.Errorf("worker: doctrine readiness fits of user %d: %v", userID, err)
		return
	}
	stamps, err := app.queries.ListSnapshotStamps(ctx, db.ListSnapshotStampsParams{CharacterIds: charIDs, Kinds: []string{esi.SnapSkills}})
	if err != nil {
		logging.Errorf("worker: doctrine readiness stamps for user %d: %v", userID, err)
		return
	}
	var newest time.Time
	for _, s := range stamps {
		if s.FetchedAt.After(newest) {
			newest = s.FetchedAt
		}
	}
	idParts := make([]string, len(charIDs))
	for i, id := range charIDs {
		idParts[i] = strconv.FormatInt(id, 10)
	}
	inputs := "chars=" + strings.Join(idParts, ",") + ";skills=" + newest.UTC().Format(time.RFC3339Nano) +
		";owners=" + strings.Join(keys, ",") + ";fits=" + fp
	if fold, err := app.queries.GetDoctrineReadinessFold(ctx, userID); err == nil {
		if fold.Inputs == inputs {
			return // the stored verdicts already cover these inputs
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		logging.Errorf("worker: doctrine readiness fold of user %d: %v", userID, err)
		return
	}

	var fits []readinessFit
	if len(keys) > 0 {
		rows, err := app.queries.ListDoctrineFitsForOwners(ctx, keys)
		if err != nil {
			logging.Errorf("worker: doctrine readiness fit list of user %d: %v", userID, err)
			return
		}
		for _, r := range rows {
			var doc fitview.FitDoc
			if r.ItemsJson != "" && json.Unmarshal([]byte(r.ItemsJson), &doc) != nil {
				continue // a fit the fitting page cannot read either
			}
			fits = append(fits, readinessFit{ID: r.ID, DoctrineID: r.DoctrineID, Types: fitTypes(r.ShipTypeID, doc)})
		}
	}
	var pilots []readinessPilot
	for _, id := range charIDs {
		var skills esi.Skills
		if !app.LoadSnapshot(ctx, id, esi.SnapSkills, &skills) {
			continue // skills not known yet: neither ready nor missing
		}
		p := readinessPilot{ID: id, Levels: fitview.ActiveSkillLevels(skills), SP: make(map[int64]int64, len(skills.Skills))}
		for _, s := range skills.Skills {
			p.SP[s.SkillID] = s.SkillpointsInSkill
		}
		pilots = append(pilots, p)
	}
	reqs, rank, ok := app.readinessRequirements(ctx, fits)
	if !ok {
		return
	}
	fitRows, docRows := foldDoctrineReadiness(fits, pilots, reqs, rank, func(id int64) string { return app.typeNameOrID(ctx, id) })

	now := time.Now().UTC()
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		logging.Errorf("worker: doctrine readiness of user %d: begin: %v", userID, err)
		return
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	if err := q.DeleteDoctrineFitReadiness(ctx, userID); err != nil {
		logging.Errorf("worker: doctrine readiness of user %d: clear fits: %v", userID, err)
		return
	}
	if err := q.DeleteDoctrineReadiness(ctx, userID); err != nil {
		logging.Errorf("worker: doctrine readiness of user %d: clear doctrines: %v", userID, err)
		return
	}
	for _, f := range fitRows {
		if err := q.InsertDoctrineFitReadiness(ctx, db.InsertDoctrineFitReadinessParams{
			UserID: userID, FitID: f.FitID, DoctrineID: f.DoctrineID, ReadyIds: f.Ready,
			NearCharacter: f.Near.CharacterID, NearMissing: f.Near.Missing, NearMissingN: int32(f.Near.MissingN), NearSp: f.Near.SP,
			ComputedAt: now,
		}); err != nil {
			logging.Errorf("worker: doctrine readiness of user %d: fit %d: %v", userID, f.FitID, err)
			return
		}
	}
	for _, d := range docRows {
		if err := q.InsertDoctrineReadiness(ctx, db.InsertDoctrineReadinessParams{
			UserID: userID, DoctrineID: d.DoctrineID, Fits: int32(d.Fits), FitsFlyable: int32(d.FitsFlyable),
			Pilots: int32(d.Pilots), PilotsReady: int32(d.Ready),
			NearCharacter: d.Near.CharacterID, NearFit: d.Near.FitID, NearMissing: d.Near.Missing,
			NearMissingN: int32(d.Near.MissingN), NearSp: d.Near.SP, ComputedAt: now,
		}); err != nil {
			logging.Errorf("worker: doctrine readiness of user %d: doctrine %d: %v", userID, d.DoctrineID, err)
			return
		}
	}
	if err := q.UpsertDoctrineReadinessFold(ctx, db.UpsertDoctrineReadinessFoldParams{UserID: userID, Inputs: inputs, ComputedAt: now}); err != nil {
		logging.Errorf("worker: doctrine readiness of user %d: watermark: %v", userID, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		logging.Errorf("worker: doctrine readiness of user %d: commit: %v", userID, err)
	}
}

// readinessRequirements reads the skill requirements of every type the
// fits carry, prerequisites included, and the ranks of every skill
// they reach.
func (app *Application) readinessRequirements(ctx context.Context, fits []readinessFit) (map[int64][]db.SdeRequirement, map[int64]float64, bool) {
	reqs := map[int64][]db.SdeRequirement{}
	rank := map[int64]float64{}
	asked := map[int64]bool{}
	var pending []int64
	for _, f := range fits {
		for _, id := range f.Types {
			if !asked[id] {
				asked[id] = true
				pending = append(pending, id)
			}
		}
	}
	skills := map[int64]bool{}
	for round := 0; round < readinessRounds && len(pending) > 0; round++ {
		batch, err := app.queries.ListSDERequirementsByTypes(ctx, pending)
		if err != nil {
			logging.Errorf("worker: doctrine readiness requirements: %v", err)
			return nil, nil, false
		}
		var next []int64
		for _, r := range batch {
			reqs[r.TypeID] = append(reqs[r.TypeID], r)
			skills[r.SkillTypeID] = true
			if !asked[r.SkillTypeID] {
				asked[r.SkillTypeID] = true
				next = append(next, r.SkillTypeID)
			}
		}
		pending = next
	}
	if len(skills) > 0 {
		ids := make([]int64, 0, len(skills))
		for id := range skills {
			ids = append(ids, id)
		}
		rows, err := app.queries.ListSDESkillRanks(ctx, ids)
		if err != nil {
			logging.Errorf("worker: doctrine readiness skill ranks: %v", err)
			return nil, nil, false
		}
		for _, r := range rows {
			rank[r.TypeID] = r.Rank
		}
	}
	return reqs, rank, true
}
