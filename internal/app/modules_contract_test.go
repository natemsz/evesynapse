package app

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/app/module"
)

// withModules replaces the registry with just these modules for one
// test and restores it afterwards, so the test sees only what it set up.
func withModules(t *testing.T, only ...module.Module) {
	t.Helper()
	before := registeredModules
	registeredModules = only
	t.Cleanup(func() { registeredModules = before })
}

// TestNotificationDeliveryFansOut: the notifier offers every
// notification to every registered deliverer; each decides for itself,
// and one reporting delivered (or panicking) never stops the others.
func TestNotificationDeliveryFansOut(t *testing.T) {
	var seen []string
	deliverer := func(name string, delivered bool) module.Deliverer {
		return func(ctx context.Context, deps module.Deps, userID int64, n module.Notification) bool {
			seen = append(seen, name+":"+n.Messages[0].Body)
			return delivered
		}
	}
	panicker := func(context.Context, module.Deps, int64, module.Notification) bool { panic("a channel failed") }
	withModules(t,
		module.Module{ID: "chan-a", Deliverers: []module.Deliverer{deliverer("a", true)}},
		module.Module{ID: "chan-b", Deliverers: []module.Deliverer{panicker, deliverer("b", false)}},
		module.Module{ID: "chan-c", Deliverers: []module.Deliverer{deliverer("c", true)}},
	)
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	app.deliverToModules(context.Background(), 1, []notifyEvent{{Kind: "skill", Title: "Skill done", URL: "/skills/"}})
	if got := strings.Join(seen, ","); got != "a:Skill done,b:Skill done,c:Skill done" {
		t.Fatalf("deliverers saw %q; want all three, in order, whatever each reported", got)
	}
	// Nothing to deliver: nobody is asked.
	seen = nil
	app.deliverToModules(context.Background(), 1, nil)
	if len(seen) != 0 {
		t.Errorf("an empty batch was offered: %v", seen)
	}
}

// TestModuleJobsRunByPhaseUnderTheirOwnGuard: a job runs at the phase it
// names (the market pass by default), under its own panic guard, so one
// job failing leaves the others running.
func TestModuleJobsRunByPhaseUnderTheirOwnGuard(t *testing.T) {
	var ran []string
	job := func(name string, phase module.JobPhase, panics bool) module.WorkerJob {
		return module.WorkerJob{Name: name, After: phase, Start: func(context.Context, module.Deps) {
			ran = append(ran, name)
			if panics {
				panic("job " + name + " failed")
			}
		}}
	}
	withModules(t, module.Module{ID: "jobs", WorkerJobs: []module.WorkerJob{
		job("default", "", false),
		job("market", module.AfterMarket, false),
		job("first", module.AfterNotifications, true),
		job("second", module.AfterNotifications, false),
	}})
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	var marks []string
	mark := func(name string, _ time.Time) { marks = append(marks, name) }

	app.runModuleJobs(context.Background(), module.AfterMarket, nil)
	if got := strings.Join(ran, ","); got != "default,market" {
		t.Fatalf("market phase ran %q", got)
	}
	ran = nil
	app.runModuleJobs(context.Background(), module.AfterNotifications, mark)
	if got := strings.Join(ran, ","); got != "first,second" {
		t.Fatalf("notifications phase ran %q; the second job must run after the first panicked", got)
	}
	if got := strings.Join(marks, ","); got != "first,second" {
		t.Errorf("phases marked in the cycle timing: %q; each job is its own", got)
	}
}

// TestNotifySettingsSectionsAreContributedAndSaved: a module's sections
// land on the settings page, its Save sees the form, and a Save error
// makes the save report failure.
func TestNotifySettingsSectionsAreContributedAndSaved(t *testing.T) {
	var saved url.Values
	var saveErr error
	withModules(t, module.Module{ID: "chan-x", NotifySettings: &module.SettingsSection{
		Channel: func(context.Context, module.Deps, *http.Request, int64) template.HTML {
			return `<p id="x-channel">box</p>`
		},
		Account: func(context.Context, module.Deps, *http.Request, int64) template.HTML {
			return `<h2 id="x-account">Channel X</h2>`
		},
		Save: func(_ context.Context, _ module.Deps, _ int64, form url.Values) error {
			saved = form
			return saveErr
		},
	}})
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/notifications/settings")
	if code != http.StatusOK {
		t.Fatalf("settings page: status %d", code)
	}
	mustContain(t, "/notifications/settings", body, `<p id="x-channel">box</p>`, `<h2 id="x-account">Channel X</h2>`)
	if !strings.Contains(body[:strings.Index(body, `id="x-channel"`)], "<form") || strings.Index(body, `id="x-channel"`) > strings.Index(body, `id="x-account"`) {
		t.Error("the channel box should sit inside the form, before the account section")
	}

	// A saved form reaches the module, and a Save error is reported.
	if code, _ := postForm(t, app, cookie, "/notifications/settings", url.Values{"x_field": {"1"}}); code != http.StatusSeeOther && code != http.StatusOK {
		t.Fatalf("save: status %d", code)
	}
	if saved.Get("x_field") != "1" {
		t.Errorf("the module's Save saw %v, want the submitted form", saved)
	}
	_, body = getPage(t, app, cookie, "/notifications/settings")
	if !strings.Contains(body, "Notification settings saved") {
		t.Errorf("a clean save did not say so")
	}
	saveErr = errors.New("store down")
	_, _ = postForm(t, app, cookie, "/notifications/settings", url.Values{"x_field": {"2"}})
	_, body = getPage(t, app, cookie, "/notifications/settings")
	if !strings.Contains(body, "could not be saved") {
		t.Errorf("a failing module Save did not make the save report failure")
	}
}

type stubGate struct {
	defer_   bool
	urgent   bool
	panicNow bool
}

func (g stubGate) Defer(ctx context.Context, deps module.Deps, kind, key string, now time.Time) bool {
	if g.panicNow {
		panic("gate broke")
	}
	return g.defer_
}

func (g stubGate) UrgentHistory(ctx context.Context, deps module.Deps, regionID, typeID int64) bool {
	if g.panicNow {
		panic("gate broke")
	}
	return g.urgent
}

// TestFetchGatesFailOpen: a gate can delay a fetch but never break one.
// A gate that says "defer" defers; one that panics is read as "fetch
// normally", and does not stop a later gate from answering.
func TestFetchGatesFailOpen(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	now := time.Now()
	mod := func(id string, g module.FetchGate) module.Module {
		return module.Module{ID: id, Title: id, Requires: []string{}, Scopes: []module.Scope{}, EnhanceScopes: []module.Scope{}, FetchGates: []module.FetchGate{g}}
	}

	withModules(t)
	if app.deferFetch(ctx, module.FetchKindTypeName, "1", now) || app.urgentHistory(ctx, marketKey{RegionID: 1, TypeID: 2}) {
		t.Error("with no gate registered a fetch was deferred")
	}
	withModules(t, mod("a", stubGate{panicNow: true}))
	if app.deferFetch(ctx, module.FetchKindTypeName, "1", now) || app.urgentHistory(ctx, marketKey{RegionID: 1, TypeID: 2}) {
		t.Error("a panicking gate deferred a fetch; it must read as fetch normally")
	}
	withModules(t, mod("a", stubGate{panicNow: true}), mod("b", stubGate{defer_: true, urgent: true}))
	if !app.deferFetch(ctx, module.FetchKindTypeName, "1", now) || !app.urgentHistory(ctx, marketKey{RegionID: 1, TypeID: 2}) {
		t.Error("a working gate after a broken one did not answer")
	}
}
