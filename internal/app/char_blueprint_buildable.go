package app

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
)

// Blueprint buildability (schema 122): for every blueprint an
// account's characters own, whether one manufacturing run can start
// right now. Skills: every skill the blueprint asks for, trained to
// its level. Materials: one run's direct inputs at the blueprint's
// ME. Two verdicts: the owner's (its skills, its own hangars) and the
// account's (any character's skills, every hangar pooled). The
// worker folds them from the stored snapshots; the Blueprints page
// only reads them.

// buildPilot is one character's side of a fold: its trained skills
// and its stock.
type buildPilot struct {
	trained map[int64]int
	stock   map[int64]int64
}

// buildCheck is one verdict: whether the skills and materials are
// there, and what is missing.
type buildCheck struct {
	Skills, Mats bool
	Missing      string // "Mechanical Engineering IV, Industry V"
	Short        int64  // materials short of one run
}

// blueprintVerdict is one owned blueprint's two verdicts.
type blueprintVerdict struct {
	CharacterID, ItemID, TypeID int64
	TotalMats                   int64
	Own, Any                    buildCheck
}

// checkBuild judges one blueprint (at its ME) against one set of
// trained skills and one stock.
func checkBuild(me int64, inputs []db.SdeBlueprintMaterial, reqs []db.SdeBlueprintSkill,
	trained map[int64]int, stock map[int64]int64, skillName func(int64) string) buildCheck {
	c := buildCheck{Skills: true}
	var missing []string
	for _, s := range reqs {
		if trained[s.SkillTypeID] < int(s.Level) {
			c.Skills = false
			missing = append(missing, skillName(s.SkillTypeID)+" "+esi.RomanLevel(int(s.Level)))
		}
	}
	c.Missing = strings.Join(missing, ", ")
	for _, m := range inputs {
		perRun := int64(math.Ceil(float64(m.Quantity) * (1 - float64(me)/100)))
		if perRun < 1 {
			perRun = 1
		}
		if stock[m.MaterialTypeID] < perRun {
			c.Short++
		}
	}
	c.Mats = c.Short == 0
	return c
}

// foldBlueprintBuildable judges every owned blueprint for its owner
// and for the account. The account's skills verdict is the best
// character's (fewest missing skills); its materials verdict reads
// the pooled stock. Blueprints with no manufacturing materials in
// the static data (formulas, unknown types) get no verdict.
func foldBlueprintBuildable(owned map[int64]esi.Blueprints, pilots map[int64]buildPilot, pooled map[int64]int64,
	mats map[int64][]db.SdeBlueprintMaterial, reqs map[int64][]db.SdeBlueprintSkill,
	skillName func(int64) string) []blueprintVerdict {
	ids := make([]int64, 0, len(pilots))
	for id := range pilots {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []blueprintVerdict
	for charID, bps := range owned {
		for _, bp := range bps {
			inputs := mats[bp.TypeID]
			if len(inputs) == 0 {
				continue
			}
			v := blueprintVerdict{CharacterID: charID, ItemID: bp.ItemID, TypeID: bp.TypeID, TotalMats: int64(len(inputs))}
			p := pilots[charID]
			v.Own = checkBuild(bp.MaterialEfficiency, inputs, reqs[bp.TypeID], p.trained, p.stock, skillName)
			v.Any = checkBuild(bp.MaterialEfficiency, inputs, reqs[bp.TypeID], p.trained, pooled, skillName)
			if !v.Any.Skills {
				for _, id := range ids {
					c := checkBuild(bp.MaterialEfficiency, nil, reqs[bp.TypeID], pilots[id].trained, nil, skillName)
					if c.Skills {
						v.Any.Skills, v.Any.Missing = true, ""
						break
					}
					if strings.Count(c.Missing, ",") < strings.Count(v.Any.Missing, ",") {
						v.Any.Missing = c.Missing
					}
				}
			}
			out = append(out, v)
		}
	}
	return out
}

// refreshBlueprintBuildable folds the verdicts for the account that
// owns ch, when any of its characters' blueprint, asset or skill
// snapshots advanced past the last fold or its characters changed.
// The watermark reads snapshot stamps only, so the per-character
// worker passes that find nothing new stay cheap. Best-effort like
// the asset values fold beside it: a failure logs and leaves the
// last verdicts in place.
func (app *Application) refreshBlueprintBuildable(ctx context.Context, ch db.Character) {
	chars, err := app.queries.ListCharactersByUser(ctx, ch.UserID)
	if err != nil || len(chars) == 0 {
		return
	}
	charIDs := make([]int64, len(chars))
	keyParts := make([]string, len(chars))
	for i, c := range chars {
		charIDs[i] = c.CharacterID
	}
	sort.Slice(charIDs, func(i, j int) bool { return charIDs[i] < charIDs[j] })
	for i, id := range charIDs {
		keyParts[i] = strconv.FormatInt(id, 10)
	}
	key := strings.Join(keyParts, ",")

	kinds := []string{esi.SnapBlueprints, esi.SnapAssets, esi.SnapSkills}
	stamps, err := app.queries.ListSnapshotStamps(ctx, db.ListSnapshotStampsParams{CharacterIds: charIDs, Kinds: kinds})
	if err != nil {
		logging.Errorf("worker: blueprint buildable stamps for user %d: %v", ch.UserID, err)
		return
	}
	var newest time.Time
	hasBlueprints := false
	for _, s := range stamps {
		if s.FetchedAt.After(newest) {
			newest = s.FetchedAt
		}
		if s.Kind == esi.SnapBlueprints {
			hasBlueprints = true
		}
	}
	if !hasBlueprints {
		return // no blueprint snapshot on the account yet
	}
	if fold, err := app.queries.GetBlueprintBuildableFold(ctx, ch.UserID); err == nil {
		if fold.Characters == key && !newest.After(fold.ComputedAt) {
			return // the stored verdicts already cover these snapshots
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		logging.Errorf("worker: blueprint buildable fold for user %d: %v", ch.UserID, err)
		return
	}

	owned := make(map[int64]esi.Blueprints)
	pilots := make(map[int64]buildPilot, len(chars))
	pooled := make(map[int64]int64)
	typeSet := make(map[int64]bool)
	for _, id := range charIDs {
		p := buildPilot{trained: map[int64]int{}, stock: map[int64]int64{}}
		var bps esi.Blueprints
		if app.LoadSnapshot(ctx, id, esi.SnapBlueprints, &bps) {
			owned[id] = bps
			for _, bp := range bps {
				typeSet[bp.TypeID] = true
			}
		}
		var assets []esi.Asset
		if app.LoadSnapshot(ctx, id, esi.SnapAssets, &assets) {
			for _, a := range assets {
				if a.Quantity > 0 {
					p.stock[a.TypeID] += a.Quantity
					pooled[a.TypeID] += a.Quantity
				}
			}
		}
		var skills esi.Skills
		if app.LoadSnapshot(ctx, id, esi.SnapSkills, &skills) {
			for _, s := range skills.Skills {
				p.trained[s.SkillID] = s.TrainedSkillLevel
			}
		}
		pilots[id] = p
	}
	typeIDs := make([]int64, 0, len(typeSet))
	for id := range typeSet {
		typeIDs = append(typeIDs, id)
	}
	mats := make(map[int64][]db.SdeBlueprintMaterial)
	rows, err := app.queries.ListSDEBlueprintMaterialsForBlueprints(ctx, typeIDs)
	if err != nil {
		logging.Errorf("worker: blueprint buildable materials for user %d: %v", ch.UserID, err)
		return
	}
	for _, r := range rows {
		mats[r.BlueprintTypeID] = append(mats[r.BlueprintTypeID], r)
	}
	reqs := make(map[int64][]db.SdeBlueprintSkill)
	skillRows, err := app.queries.ListSDEBlueprintSkillsForBlueprints(ctx, typeIDs)
	if err != nil {
		logging.Errorf("worker: blueprint buildable skills for user %d: %v", ch.UserID, err)
		return
	}
	for _, r := range skillRows {
		reqs[r.BlueprintTypeID] = append(reqs[r.BlueprintTypeID], r)
	}

	verdicts := foldBlueprintBuildable(owned, pilots, pooled, mats, reqs,
		func(id int64) string { return app.typeNameOrID(ctx, id) })
	now := time.Now().UTC()
	for _, v := range verdicts {
		if err := app.queries.UpsertBlueprintBuildable(ctx, db.UpsertBlueprintBuildableParams{
			UserID: ch.UserID, CharacterID: v.CharacterID, ItemID: v.ItemID, BlueprintType: v.TypeID,
			TotalMaterials: v.TotalMats,
			HasSkills:      v.Own.Skills, HasMaterials: v.Own.Mats, MissingSkills: v.Own.Missing, ShortMaterials: v.Own.Short,
			AnySkills: v.Any.Skills, AnyMaterials: v.Any.Mats, AnyMissingSkills: v.Any.Missing, AnyShortMaterials: v.Any.Short,
			ComputedAt: now,
		}); err != nil {
			logging.Errorf("worker: blueprint buildable store for user %d: %v", ch.UserID, err)
			return
		}
	}
	if err := app.queries.DeleteBlueprintBuildableOlder(ctx, db.DeleteBlueprintBuildableOlderParams{UserID: ch.UserID, FoldAt: now}); err != nil {
		logging.Errorf("worker: blueprint buildable sweep for user %d: %v", ch.UserID, err)
		return
	}
	if err := app.queries.UpsertBlueprintBuildableFold(ctx, db.UpsertBlueprintBuildableFoldParams{UserID: ch.UserID, Characters: key, ComputedAt: now}); err != nil {
		logging.Errorf("worker: blueprint buildable fold record for user %d: %v", ch.UserID, err)
	}
}
