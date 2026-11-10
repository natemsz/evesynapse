package fit

import (
	"context"
	"fmt"
	"sync"

	db "evesynapse/internal/db/sqlc"
)

// skillDogma is every published skill's dogma, read once and merged
// into each snapshot that computes for a pilot, so All V and a
// pilot's own trained skills reach every bonus (Drone Interfacing,
// Warhead Upgrades, Surgical Strike), not only the skills the fit's
// items name. The rows are static until a new SDE lands.
var skillDogma struct {
	mu   sync.Mutex
	snap *Snapshot
	ids  []int64
}

// ResetSkillDogma drops the cached skill dogma; the next
// WithAllSkills reads it again. Called when a new SDE is imported.
func ResetSkillDogma() {
	skillDogma.mu.Lock()
	skillDogma.snap, skillDogma.ids = nil, nil
	skillDogma.mu.Unlock()
}

// WithAllSkills merges every published skill's dogma into snap
// (types already loaded keep their rows) and returns the skills'
// type IDs. The merged maps are shared read-only with the cache.
func WithAllSkills(ctx context.Context, q *db.Queries, snap *Snapshot) ([]int64, error) {
	skills, ids, err := cachedSkillDogma(ctx, q)
	if err != nil {
		return nil, err
	}
	for id, attrs := range skills.Attrs {
		if _, ok := snap.Attrs[id]; !ok {
			snap.Attrs[id] = attrs
		}
	}
	for id, g := range skills.Groups {
		if _, ok := snap.Groups[id]; !ok {
			snap.Groups[id] = g
		}
	}
	for id, effs := range skills.TypeEffects {
		if _, ok := snap.TypeEffects[id]; !ok {
			snap.TypeEffects[id] = effs
		}
	}
	for id, reqs := range skills.Requirements {
		if _, ok := snap.Requirements[id]; !ok {
			snap.Requirements[id] = reqs
		}
	}
	for id, eff := range skills.Effects {
		if _, ok := snap.Effects[id]; !ok {
			snap.Effects[id] = eff
		}
	}
	for id, meta := range skills.Meta {
		if _, ok := snap.Meta[id]; !ok {
			snap.Meta[id] = meta
		}
	}
	return ids, nil
}

func cachedSkillDogma(ctx context.Context, q *db.Queries) (*Snapshot, []int64, error) {
	skillDogma.mu.Lock()
	defer skillDogma.mu.Unlock()
	if skillDogma.snap != nil {
		return skillDogma.snap, skillDogma.ids, nil
	}
	rows, err := q.ListSDESkillCatalog(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list skills: %w", err)
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.TypeID)
	}
	snap, err := LoadSnapshot(ctx, q, ids)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) > 0 {
		skillDogma.snap, skillDogma.ids = snap, ids
	}
	return snap, ids, nil
}
