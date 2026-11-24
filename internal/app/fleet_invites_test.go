package app

import (
	"database/sql"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestFleetInvitesGoOutAsTheBossAndShowHowEachWent: those running a
// fleet queue invites for pilots coming and not in it, the ready ones
// or all, once the character the fleet is read through allows fleet
// invites; the fleet lane sends each through ESI as that character and
// keeps how it went, a refusal included. Nobody else can queue one.
func TestFleetInvitesGoOutAsTheBossAndShowHowEachWent(t *testing.T) {
	const ready, coming = 95000002, 95000003
	fleet := &fleetESI{
		fleetStatus: 200, fleet: `{"fleet_id":777,"fleet_boss_id":90000001,"role":"fleet_commander"}`,
		membersStatus: 200, members: fleetOfThree, refuse: map[int64]int{coming: 422},
	}
	f := newFleetFixture(t, fleet)
	now := time.Now().UTC()
	id := f.create(now.Add(-10*time.Minute).Truncate(time.Minute), "Running")
	if stored, limited := f.app.captureOpAttendance(f.ctx, now); stored != 3 || limited {
		t.Fatalf("read %d pilots, limited=%v; want the fleet of three", stored, limited)
	}
	for _, char := range []int64{ready, coming} {
		if err := f.q.UpsertOpSignup(f.ctx, db.UpsertOpSignupParams{OpID: id, CharacterID: char, Response: opYes, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.q.SetOpSignupReady(f.ctx, db.SetOpSignupReadyParams{ReadyAt: sql.NullTime{Time: now, Valid: true}, OpID: id, CharacterID: ready}); err != nil {
		t.Fatal(err)
	}
	path := "/fleets/" + strconv.FormatInt(id, 10) + "/invite"
	queued := func() map[int64]db.ListFleetInvitesRow {
		t.Helper()
		rows, err := f.q.ListFleetInvites(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]db.ListFleetInvitesRow{}
		for _, r := range rows {
			out[r.CharacterID] = r
		}
		return out
	}

	f.post(f.director, path, url.Values{"who": {"ready"}})
	if len(queued()) != 0 {
		t.Fatal("an invite was queued before the fleet's boss allowed fleet invites")
	}
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE characters SET scopes = scopes || ' ' || $1 WHERE character_id = $2`, fleetInviteScope, fixtureCharA); err != nil {
		t.Fatal(err)
	}
	f.post(f.other, path, url.Values{"who": {"coming"}})
	if len(queued()) != 0 {
		t.Fatal("someone not running the fleet queued invites")
	}

	f.post(f.director, path, url.Values{"who": {"ready"}})
	f.app.sendFleetInvites(f.ctx, time.Now())
	if !slices.Equal(fleet.invited, []int64{ready}) {
		t.Fatalf("invited %v, want only the pilot ready for it", fleet.invited)
	}
	f.post(f.director, path, url.Values{"who": {"coming"}})
	f.app.sendFleetInvites(f.ctx, time.Now())
	got := queued()
	if !got[ready].SentAt.Valid || got[coming].SentAt.Valid || !got[coming].FailedAt.Valid {
		t.Fatalf("invites = %+v, want the ready pilot sent and the other refused", got)
	}
	_, _, body := f.get(f.director, fleetCompositionPath(id))
	mustContain(t, "invite panel", body, "Invite sent at", "EVE refused the invite")
}
