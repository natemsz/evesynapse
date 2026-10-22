package app

// The notification settings page, as restructured: the channels
// under one themed header (in-app, Discord, browsers), the kinds
// under another, the per-browser panel in its own Browser
// notifications section at the bottom, above the Discord section,
// and the Discord direct-message switch in the channels list
// only. The Discord section below the form connects the account;
// it carries no switch.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// indexOf fails the test when the needle is not on the page, and
// returns where it is: the pins below are about what sits where,
// not only about what is present.
func indexOf(t *testing.T, body, needle string) int {
	t.Helper()
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("page does not contain %q", needle)
	}
	return i
}

func TestNotificationSettingsChannelsAndTypes(t *testing.T) {
	f := newNotifyFixture(t)
	f.enableDiscord()
	f.enablePush()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	cookie, _ = f.connect(cookie, "alice")
	// Direct messages on: the Discord box renders ticked.
	if err := f.q.SetDiscordDMNotifications(f.ctx, db.SetDiscordDMNotificationsParams{UserID: f.userID, DmNotifications: true}); err != nil {
		t.Fatal(err)
	}
	// One subscribed browser, so the panel shows its count line.
	if code, msg := f.postJSON(cookie, "/notifications/push/subscribe", browserSubscription(t, fcm+"channels-one")); code != http.StatusNoContent {
		t.Fatalf("subscribe: %d %q", code, msg)
	}

	_, body := getPage(t, f.app, cookie, "/notifications/settings")
	mustContain(t, "settings channels", body,
		"<h2>Notification Channels</h2>",
		"<h2>Notification Types</h2>",
		"On Discord <small>(direct messages to pilot-alice)</small>",
		`<input type="checkbox" name="channel_desktop" value="1" checked>`,
		"<h2>Browser notifications</h2>",
		"1 browser on this account receives notifications.",
		`<h2 id="discord">Discord</h2>`,
	)
	channels := indexOf(t, body, "<h2>Notification Channels</h2>")
	types := indexOf(t, body, "<h2>Notification Types</h2>")
	browser := indexOf(t, body, "<h2>Browser notifications</h2>")
	discord := indexOf(t, body, `<h2 id="discord">Discord</h2>`)
	if channels > types || types > browser || browser > discord {
		t.Fatal("the sections are out of order: channels, types, browser, discord")
	}
	// The channels list carries the page's only Discord box,
	// before the types: the Discord section below the form
	// connects the account and has no switch of its own.
	if first := indexOf(t, body, `name="discord_dm"`); first > types {
		t.Fatal("no Discord box in the channels list")
	}
	if n := strings.Count(body, `name="discord_dm"`); n != 1 {
		t.Fatalf("%d discord_dm boxes on the page, want exactly 1 (the channels list's)", n)
	}
	if strings.Contains(body, "Also send these to me on Discord") {
		t.Fatal("the Discord section still carries its own direct-message switch")
	}
	// The per-browser panel lives in the Browser notifications
	// section, and only there: below the types, above Discord.
	if panel := indexOf(t, body, `id="push-panel"`); panel < browser || panel > discord {
		t.Fatal("the browser panel sits outside the Browser notifications section")
	}
	if n := strings.Count(body, `id="push-panel"`); n != 1 {
		t.Fatalf("%d browser panels on the page, want 1", n)
	}

	// The page's own Save stores what the channels box says: the
	// form with the box unticked turns direct messages off, and
	// ticking it turns them on.
	settings := func(dm bool) url.Values {
		form := url.Values{
			"channels_shown": {"1"}, "channel_inapp": {"1"}, "channel_desktop": {"1"},
			"discord_shown": {"1"},
		}
		for _, kind := range notifyKindList() {
			form.Add("kind", kind.ID)
			if !kind.Account {
				form.Add("char."+kind.ID, "90000001")
			}
		}
		if dm {
			form.Set("discord_dm", "1")
		}
		return form
	}
	dm := func() bool {
		t.Helper()
		link, err := f.q.GetDiscordLink(f.ctx, f.userID)
		if err != nil {
			t.Fatal(err)
		}
		return link.DmNotifications
	}
	if code, _ := f.post(cookie, "/notifications/settings", settings(false)); code != http.StatusSeeOther || dm() {
		t.Fatalf("save unticked: %d, dm on = %v; want 303 and off", code, dm())
	}
	if code, _ := f.post(cookie, "/notifications/settings", settings(true)); code != http.StatusSeeOther || !dm() {
		t.Fatalf("save ticked: %d, dm on = %v; want 303 and on", code, dm())
	}
}

// Without a Discord link (but an install that offers the linking)
// the channels carry the connect row instead of a box, above the
// types; without push, the Browser notifications section carries
// the not-set-up line where the browser controls would be, below
// the types.
func TestNotificationSettingsChannelsUnlinkedAndNoPush(t *testing.T) {
	f := newNotifyFixture(t)
	f.enableDiscord() // the linking is offered; the account has not linked
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)

	_, body := getPage(t, f.app, cookie, "/notifications/settings")
	mustContain(t, "settings channels (unlinked, no push)", body,
		"<h2>Notification Channels</h2>",
		"On Discord <small>(not connected yet: <a href=\"#discord\">connect it under Discord, below</a>)</small>",
		"<h2>Notification Types</h2>",
		"<h2>Browser notifications</h2>",
		"Browser notifications are not set up on this server.",
	)
	types := indexOf(t, body, "<h2>Notification Types</h2>")
	if i := indexOf(t, body, "connect it under Discord, below"); i > types {
		t.Fatal("the Discord connect row sits outside the channels")
	}
	browser := indexOf(t, body, "<h2>Browser notifications</h2>")
	if i := indexOf(t, body, "Browser notifications are not set up on this server."); i < browser {
		t.Fatal("the browser not-set-up line sits outside the Browser notifications section")
	}
	if strings.Contains(body, `name="discord_dm"`) {
		t.Fatal("a Discord box is offered to an account with no link")
	}
	if strings.Contains(body, `id="push-panel"`) {
		t.Fatal("the browser panel is offered with push off")
	}
}
