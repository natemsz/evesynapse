package app

// The character sheet's trained-since-last-visit banner: it reads
// the notification store's skill events for the pilot on show, so
// what it needs is rows in the store and a sheet that renders --
// no worker pass, no ESI.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// trainedBannerFixture is an app with one account, the pilot on
// show (fixtureCharA), and a rendered sheet for it.
type trainedBannerFixture struct {
	t      *testing.T
	app    *Application
	q      *db.Queries
	ctx    context.Context
	userID int64
	cookie *http.Cookie
}

func newTrainedBannerFixture(t *testing.T) *trainedBannerFixture {
	t.Helper()
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	// The sheet's sections read snapshots; seed them empty so
	// nothing reaches for live ESI (the transport refuses).
	for kind, payload := range map[string]any{
		esi.SnapSkills:     esi.Skills{},
		esi.SnapSkillqueue: esi.Skillqueue{},
		esi.SnapAttributes: esi.Attributes{},
		esi.SnapClones:     esi.Clones{},
		esi.SnapImplants:   esi.Implants{},
		esi.SnapOnline:     esi.Online{},
		esi.SnapLocation:   esi.Location{},
		esi.SnapShip:       esi.Ship{},
		esi.SnapFatigue:    esi.Fatigue{},
		esi.SnapProfile:    esi.Character{},
		esi.SnapWallet:     0.0,
		esi.SnapCorpInfo:   esi.Corporation{},
	} {
		seedSnapshot(t, q, fixtureCharA, kind, payload)
	}
	return &trainedBannerFixture{
		t: t, app: app, q: q, ctx: ctx, userID: user.ID,
		cookie: sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo"),
	}
}

func (f *trainedBannerFixture) sheet(characterID int64) string {
	f.t.Helper()
	code, body := getPage(f.t, f.app, f.cookie, fmt.Sprintf("/character/?character=%d", characterID))
	if code != http.StatusOK {
		f.t.Fatalf("GET /character/?character=%d = %d", characterID, code)
	}
	return body
}

// addSkill stores one skill event in the producer's exact title
// format (notifySkillEvents).
func (f *trainedBannerFixture) addSkill(userID, characterID int64, charName, skill string, level int) {
	f.t.Helper()
	title := fmt.Sprintf("%s finished training %s %s", charName, skill, esi.RomanLevel(level))
	if _, err := f.q.InsertNotification(f.ctx, db.InsertNotificationParams{
		UserID: userID, CharacterID: characterID, Kind: notifySkill, Title: title,
		Url:       fmt.Sprintf("/character/?character=%d", characterID),
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		f.t.Fatal(err)
	}
}

// markSkillRead marks the account's skill rows read at the given
// moment, as the notification click-through does.
func (f *trainedBannerFixture) markSkillRead(at time.Time) {
	f.t.Helper()
	if err := f.q.MarkNotificationsReadByKind(f.ctx, db.MarkNotificationsReadByKindParams{
		ReadAt: sql.NullTime{Time: at, Valid: true}, UserID: f.userID, Kind: notifySkill,
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *trainedBannerFixture) wantBanner(body, want string) {
	f.t.Helper()
	mustContain(f.t, "/character/", body,
		`<p class="notice" role="status">Trained since your last visit: `+want+`</p>`)
}

func (f *trainedBannerFixture) wantNoBanner(body string) {
	f.t.Helper()
	if strings.Contains(body, "Trained since your last visit") {
		f.t.Fatal("the banner is shown with nothing trained to announce")
	}
}

// The arrival the banner is built for: the visitor followed the
// skill notification, whose click-through marked the kind read on
// the way in. The banner still names what finished, in the order
// the skills completed.
func TestCharacterTrainedBannerJustRead(t *testing.T) {
	f := newTrainedBannerFixture(t)
	f.addSkill(f.userID, fixtureCharA, "Fixture Ceo", "Alpha Skill", 5)
	f.addSkill(f.userID, fixtureCharA, "Fixture Ceo", "Test Gunnery", 4)
	f.markSkillRead(time.Now().UTC().Add(-2 * time.Minute))
	f.wantBanner(f.sheet(fixtureCharA), "Alpha Skill V · Test Gunnery IV")
}

// Unread skill events are news on any visit, notification or not.
func TestCharacterTrainedBannerUnread(t *testing.T) {
	f := newTrainedBannerFixture(t)
	f.addSkill(f.userID, fixtureCharA, "Fixture Ceo", "Alpha Skill", 5)
	f.wantBanner(f.sheet(fixtureCharA), "Alpha Skill V")
}

// No skill events, no banner -- not even its markup. Other kinds
// of news do not stand in for it.
func TestCharacterTrainedBannerNone(t *testing.T) {
	f := newTrainedBannerFixture(t)
	if _, err := f.q.InsertNotification(f.ctx, db.InsertNotificationParams{
		UserID: f.userID, CharacterID: fixtureCharA, Kind: notifyMail,
		Title: "Fixture Ceo has new mail", Url: "/mail/?character=90000001",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	f.wantNoBanner(f.sheet(fixtureCharA))
}

// The banner is per pilot: a second pilot's finished skills never
// show on this sheet, and do show on their own.
func TestCharacterTrainedBannerOtherPilot(t *testing.T) {
	f := newTrainedBannerFixture(t)
	seedCharacter(t, f.q, f.userID, fixtureCharB, "Fixture Alt")
	seedSnapshot(t, f.q, fixtureCharB, esi.SnapOnline, esi.Online{})
	f.addSkill(f.userID, fixtureCharB, "Fixture Alt", "Beta Skill", 3)
	f.wantNoBanner(f.sheet(fixtureCharA))
	f.wantBanner(f.sheet(fixtureCharB), "Beta Skill III")
}

// Another account's events for the same character id are not this
// visitor's news.
func TestCharacterTrainedBannerOtherUser(t *testing.T) {
	f := newTrainedBannerFixture(t)
	other, err := f.q.CreateUser(f.ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	f.addSkill(other.ID, fixtureCharA, "Fixture Ceo", "Alpha Skill", 5)
	f.wantNoBanner(f.sheet(fixtureCharA))
}

// Skill events read long ago were last visit's news, not this
// visit's.
func TestCharacterTrainedBannerStaleRead(t *testing.T) {
	f := newTrainedBannerFixture(t)
	f.addSkill(f.userID, fixtureCharA, "Fixture Ceo", "Alpha Skill", 5)
	f.markSkillRead(time.Now().UTC().Add(-3 * time.Hour))
	f.wantNoBanner(f.sheet(fixtureCharA))
}

// The parse is pinned to the producer's title format: the skill
// may contain spaces, the level is the trailing Roman numeral.
func TestTrainedEntryParsesProducerTitles(t *testing.T) {
	for level := 1; level <= 5; level++ {
		title := fmt.Sprintf("%s finished training %s %s", "Fixture Ceo", "Caldari Navy Vessel Command", esi.RomanLevel(level))
		entry, ok := trainedEntry("Fixture Ceo", title)
		if want := "Caldari Navy Vessel Command " + esi.RomanLevel(level); !ok || entry != want {
			t.Fatalf("trainedEntry(%q) = %q, %v; want %q, true", title, entry, ok, want)
		}
	}
	for _, bad := range []string{
		"Other Pilot finished training Alpha Skill V", // another pilot's row
		"Fixture Ceo has new mail",                    // another kind's text
		"Fixture Ceo finished training Alpha Skill",   // no level
		"Fixture Ceo finished training Alpha Skill VI",
		"Fixture Ceo finished training ",
	} {
		if entry, ok := trainedEntry("Fixture Ceo", bad); ok {
			t.Fatalf("trainedEntry(%q) = %q, true; want no entry", bad, entry)
		}
	}
}
