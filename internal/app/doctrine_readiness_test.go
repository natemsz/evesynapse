package app

import (
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestDoctrineReadinessFoldsOnlyTheAccountsDoctrines: an account's fold
// covers the doctrines of the corporations it belongs to and no other's.
// A pilot with every skill the fit asks for is ready; one without is
// the nearest gap, named with the skill it lacks and the skill points
// left to train.
func TestDoctrineReadinessFoldsOnlyTheAccountsDoctrines(t *testing.T) {
	f := newNotifyFixture(t)
	const ship, hullSkill, otherCorp = int64(29990), int64(30651), int64(98000009)
	for _, s := range []string{
		`INSERT INTO sde_types (type_id, name) VALUES (29990, 'Loki'), (30651, 'Minmatar Strategic Cruiser') ON CONFLICT DO NOTHING`,
		`INSERT INTO sde_requirements (type_id, skill_type_id, level) VALUES (29990, 30651, 4) ON CONFLICT DO NOTHING`,
		`INSERT INTO sde_skill_meta (type_id, rank, primary_attr, secondary_attr) VALUES (30651, 5, 0, 0) ON CONFLICT DO NOTHING`,
	} {
		if _, err := f.app.db.ExecContext(f.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	skills := func(charID int64, level int, sp int64) {
		seedSnapshot(t, f.q, charID, esi.SnapSkills, esi.Skills{Skills: []esi.Skill{
			{SkillID: hullSkill, ActiveSkillLevel: level, TrainedSkillLevel: level, SkillpointsInSkill: sp}}})
	}
	f.joinCorp(f.ch.CharacterID, discordCorp)
	skills(f.ch.CharacterID, 5, 1280000)
	seedCharacter(t, f.q, f.userID, 90000005, "Fixture Alt")
	f.joinCorp(90000005, discordCorp)
	skills(90000005, 3, 40000)
	other, err := f.q.CreateUser(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, f.q, other.ID, 90000003, "Fixture Stranger")
	f.joinCorp(90000003, otherCorp)
	skills(90000003, 5, 1280000)

	now := time.Now().UTC()
	doctrine := func(corp int64, name string) {
		id, err := f.q.CreateDoctrine(f.ctx, db.CreateDoctrineParams{CorporationID: corp, OwnerKind: ownerCorporation, OwnerID: corp, Visibility: "members", Name: name, CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.q.AddDoctrineFit(f.ctx, db.AddDoctrineFitParams{DoctrineID: id, Name: "Mainline " + name, ShipTypeID: ship,
			ItemsJson: `{"shipTypeId":29990,"items":[]}`, AddedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	doctrine(discordCorp, "Shield Lokis")
	doctrine(otherCorp, "Armor Lokis")
	f.app.refreshDoctrineReadiness(f.ctx, f.userID)
	f.app.refreshDoctrineReadiness(f.ctx, other.ID)

	read := func(userID int64) []db.ListDoctrineReadinessByUserRow {
		rows, err := f.q.ListDoctrineReadinessByUser(f.ctx, db.ListDoctrineReadinessByUserParams{UserID: userID, RowLimit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	mine := read(f.userID)
	if len(mine) != 1 || mine[0].Name != "Shield Lokis" {
		t.Fatalf("the account's fold holds %+v, want its own corporation's doctrine only", mine)
	}
	if r := mine[0]; r.Pilots != 2 || r.PilotsReady != 1 || r.NearCharacter != 90000005 ||
		r.NearMissing != "Minmatar Strategic Cruiser IV" || r.NearSp != spAtLevel(5, 4)-40000 {
		t.Errorf("readiness = %d of %d ready, nearest %d needs %q (%d SP); want 1 of 2, the alt needing the hull skill at IV",
			r.PilotsReady, r.Pilots, r.NearCharacter, r.NearMissing, r.NearSp)
	}
	if theirs := read(other.ID); len(theirs) != 1 || theirs[0].Name != "Armor Lokis" || theirs[0].PilotsReady != 1 {
		t.Errorf("the other account's fold holds %+v, want its own corporation's doctrine, ready", theirs)
	}
}
