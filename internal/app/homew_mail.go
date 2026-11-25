package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetMail, Title: "Mail", Category: wcatPilots, Feature: "mail", Requires: "mail",
		Href:        "/mail/",
		Description: "Unread EVE mail across your pilots, and who sent the newest.",
		Default:     wSize{1, 1},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "Unread mail across your pilots and the newest one."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "Unread mail per pilot, each with its newest subject."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The newest unread mails across your pilots."},
			{Name: "large", Sizes: "3x2 3x3", Says: "The newest unread mails beside the unread count per pilot."},
		},
		Needs:   []string{esi.SnapMail, esi.SnapMailLabels},
		Options: []widgetOption{optScope()},
		Build:   buildMailWidget,
	})
}

// mailWidgetHeaders is the most unread mails the widget lists.
const mailWidgetHeaders = 20

// mailHeaderRow is one unread mail.
type mailHeaderRow struct {
	Href     string
	Subject  string
	SenderID int64
	Sender   string // "" while the sender's name is not cached
	CharID   int64
	Pilot    string
	Ago      string
	At       string
	at       time.Time
}

// mailPilotRow is one pilot's unread count and newest unread mail.
type mailPilotRow struct {
	CharID int64
	Name   string
	Href   string
	Unread int64
	Newest *mailHeaderRow
}

// mailWidgetView is what the Mail forms draw.
type mailWidgetView struct {
	Lead    wLead
	Unread  int64
	Newest  *mailHeaderRow
	Pilots  []mailPilotRow // pilots with unread mail, most unread first
	Headers []mailHeaderRow
	Known   bool
	Empty   wEmpty
}

func buildMailWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &mailWidgetView{Empty: wEmpty{Text: "Mail has not loaded yet.", Step: "Open Mail", Href: "/mail/"}}
	now := in.Now.UTC()
	for _, b := range in.Bundles {
		if b.mailLabels == nil {
			continue
		}
		v.Known = true
		if b.mailLabels.TotalUnreadCount <= 0 {
			continue
		}
		v.Unread += b.mailLabels.TotalUnreadCount
		p := mailPilotRow{CharID: b.ch.CharacterID, Name: b.ch.Name, Unread: b.mailLabels.TotalUnreadCount,
			Href: fmt.Sprintf("/mail/?character=%d&view=1", b.ch.CharacterID)}
		if b.mailKnown {
			for _, h := range b.mail {
				if h.IsRead {
					continue
				}
				at, ok := parseRFC3339(h.Timestamp)
				if !ok {
					continue
				}
				row := app.mailHeaderRowOf(b, h, at, now)
				if p.Newest == nil || at.After(p.Newest.at) {
					r := row
					p.Newest = &r
				}
				v.Headers = append(v.Headers, row)
			}
		}
		v.Pilots = append(v.Pilots, p)
	}
	sort.SliceStable(v.Headers, func(i, j int) bool { return v.Headers[i].at.After(v.Headers[j].at) })
	if len(v.Headers) > mailWidgetHeaders {
		v.Headers = v.Headers[:mailWidgetHeaders]
	}
	if len(v.Headers) > 0 {
		v.Newest = &v.Headers[0]
	}
	sort.SliceStable(v.Pilots, func(i, j int) bool {
		if v.Pilots[i].Unread != v.Pilots[j].Unread {
			return v.Pilots[i].Unread > v.Pilots[j].Unread
		}
		return v.Pilots[i].Name < v.Pilots[j].Name
	})
	v.Lead = wLead{Label: "Unread mail", Value: strconv.FormatInt(v.Unread, 10), Href: "/mail/",
		Full: pluralCount(v.Unread, "unread mail", "unread mails")}
	if v.Unread == 0 {
		v.Empty = wEmpty{Text: "No unread mail.", Step: "Open Mail", Href: "/mail/"}
	}
	return v
}

// mailHeaderRowOf is one unread mail as a row. The sender's name comes
// from the in-process name cache only; a mail never waits on ESI.
func (app *Application) mailHeaderRowOf(b *charSnaps, h esi.MailHeader, at, now time.Time) mailHeaderRow {
	row := mailHeaderRow{
		Href:    fmt.Sprintf("/mail/?character=%d&box=inbox&mail=%d", b.ch.CharacterID, h.MailID),
		Subject: h.Subject, SenderID: h.From, CharID: b.ch.CharacterID, Pilot: b.ch.Name,
		Ago: humanDuration(now.Sub(at)) + " ago", At: formatFinish(h.Timestamp), at: at,
	}
	if row.Subject == "" {
		row.Subject = "(no subject)"
	}
	if name, ok := app.esi.CachedCharacterName(h.From); ok {
		row.Sender = name
	}
	return row
}
