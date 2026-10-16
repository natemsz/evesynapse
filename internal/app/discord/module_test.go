package discord

import "testing"

// TestModuleDeclaration: all five declarations written out, three worker
// jobs (separate passes under their own names, after the notifications),
// one notification channel and a settings section.
func TestModuleDeclaration(t *testing.T) {
	m := Module
	if m.ID != "discord" || m.Title == "" || m.Requires == nil || m.Scopes == nil || m.EnhanceScopes == nil {
		t.Fatalf("incomplete declaration: %+v", m)
	}
	if len(m.Scopes) != 0 || len(m.EnhanceScopes) != 1 || m.EnhanceScopes[0].Scope != "esi-characters.read_corporation_roles.v1" {
		t.Errorf("scopes %+v enhance %+v; want none required and the corporation roles scope as the enhancement", m.Scopes, m.EnhanceScopes)
	}
	names := map[string]bool{}
	for _, job := range m.WorkerJobs {
		names[job.Name] = true
		if job.Start == nil || job.After != "notifications" {
			t.Errorf("job %q: start %v after %q; want a start, after the notifications", job.Name, job.Start != nil, job.After)
		}
	}
	for _, want := range []string{"discord announce ops", "discord role sync", "discord kick pass"} {
		if !names[want] {
			t.Errorf("no worker job named %q (have %v)", want, names)
		}
	}
	if len(m.WorkerJobs) != 3 {
		t.Errorf("%d worker jobs, want the three passes kept separate", len(m.WorkerJobs))
	}
	if len(m.Deliverers) != 1 || m.NotifySettings == nil || m.NotifySettings.Channel == nil || m.NotifySettings.Account == nil || m.NotifySettings.Save == nil {
		t.Errorf("deliverers %d, settings %+v; want the DM channel and its three settings parts", len(m.Deliverers), m.NotifySettings)
	}
	if len(m.Nav) != 1 || m.Nav[0].Entries[0].After != "/srp/" {
		t.Errorf("nav %+v; want one entry after Ship Replacement", m.Nav)
	}
}
