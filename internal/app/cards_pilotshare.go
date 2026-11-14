package app

// ---------------------------------------------------------------------------
// The pilot share sheet: /pilot/share.png?character={id}, the large
// card the character sheet's Share button previews and hands out.
// The 1200x630 summary card (cards.go) stays the link preview; the
// sheet carries more of the public record: combat figures, the
// hulls flown most, active hours and the employment summary.
//
// Everything on it is public record or killboard data, the same
// rows the public pilot page shows. Nothing from the owner's own
// ESI scopes (wallet, location, skills, assets) is read here, so
// the sheet is the same image whoever asks for it.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"net/http"
	"strconv"
	"strings"
	"time"

	json "github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	"evesynapse/internal/card"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// pilotSheetShips is how many hulls the sheet lists.
const pilotSheetShips = 5

// pilotSheetJob is one sheet ready to draw: its content and the
// pictures to fetch into it (the portrait, then a ship icon per row).
type pilotSheetJob struct {
	key, version string
	sheet        card.Profile
	portrait     string
	shipIcons    []string
}

// handlePilotShareCard serves /pilot/share.png?character={id}.
func (app *Application) handlePilotShareCard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64)
	if err != nil || id <= 0 {
		app.serveFallbackCard(w)
		return
	}
	job, ok := app.pilotSheetJob(r.Context(), id, time.Now())
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	app.serveDrawn(w, r, job.key, job.version, time.Hour, func(ctx context.Context) ([]byte, bool, error) {
		sheet := job.sheet
		pic, complete := app.cardPicture(ctx, job.portrait)
		sheet.Picture = pic
		sheet.Ships = append([]card.ProfileRow(nil), sheet.Ships...)
		for i, url := range job.shipIcons {
			icon, ok := app.cardPicture(ctx, url)
			complete = complete && ok
			sheet.Ships[i].Icon = icon
		}
		data, err := card.ProfilePNG(sheet)
		return data, complete, err
	})
}

// pilotSheetJob reads a pilot's sheet from the stored public record
// and activity aggregates: bounded single-row reads plus the top
// hulls (LIMIT) and the 24 hour rows. A pilot without a ready
// record does not resolve; one without kills gets the record alone.
func (app *Application) pilotSheetJob(ctx context.Context, id int64, now time.Time) (pilotSheetJob, bool) {
	rec, err := app.queries.GetPilotRecord(ctx, id)
	if err != nil || rec.State != pilotStateReady || rec.Payload == "" {
		return pilotSheetJob{}, false
	}
	var p pilotPayload
	if json.Unmarshal([]byte(rec.Payload), &p) != nil || p.Profile.Name == "" {
		return pilotSheetJob{}, false
	}
	s := card.Profile{
		Kind:       "PILOT",
		Title:      p.Profile.Name,
		ShipsLabel: "FLIES MOST",
		HoursLabel: "ACTIVE HOURS · EVE TIME",
		Foot:       "evesynapse.app",
		Note:       "Public killboard and record data",
	}
	if p.Corp.Name != "" {
		line := p.Corp.Name
		if p.Corp.Ticker != "" {
			line += " [" + p.Corp.Ticker + "]"
		}
		s.Lines = append(s.Lines, line)
	}
	if p.Alliance.Name != "" {
		s.Lines = append(s.Lines, p.Alliance.Name)
	}
	sec := p.Profile.SecurityStatus
	s.Chips = append(s.Chips, card.Stat{Label: "SECURITY", Value: []card.Seg{{Text: fmt.Sprintf("%.2f", sec), Color: pilotSecColor(sec)}}})
	if born, err := time.Parse(time.RFC3339, p.Profile.Birthday); err == nil {
		s.Chips = append(s.Chips, card.Stat{Label: "BORN", Value: card.Figure(born.UTC().Format("2006-01-02"))})
		if years := int(now.Sub(born).Hours() / (24 * 365.25)); years >= 1 {
			s.Chips = append(s.Chips, card.Stat{Label: "PILOT FOR", Value: card.Figure(pluralCount(int64(years), "year", "years"))})
		}
	}
	if n := len(p.History); n > 0 {
		s.Facts = append(s.Facts, card.Detail{Label: "CORPORATIONS", Text: strconv.Itoa(n)})
		if cur := p.History[n-1]; cur.End == "" {
			if start, err := time.Parse(time.RFC3339, cur.Start); err == nil {
				s.Facts = append(s.Facts, card.Detail{Label: "IN CORP SINCE", Text: start.UTC().Format("2006-01-02")})
			}
		}
	}
	if p.FactionName != "" {
		s.Facts = append(s.Facts, card.Detail{Label: "MILITIA", Text: p.FactionName})
	}

	data := "record:" + rec.FetchedAt.Time.UTC().Format(time.RFC3339Nano)
	var shipIcons []string
	prof, err := app.queries.GetPilotActivityProfile(ctx, id)
	switch {
	case err == nil && prof.Kills+prof.Losses > 0:
		data += "|profile:" + prof.UpdatedAt.UTC().Format(time.RFC3339Nano)
		st, stErr := app.pilotAllTimeStats(ctx, id)
		if stErr != nil {
			logging.Warnf("pilot sheet %d: stats: %v", id, stErr)
		}
		s.Figures = []card.Stat{
			{Label: "KILLS", Value: card.Figure(esi.FormatInt(prof.Kills))},
			{Label: "LOSSES", Value: card.Figure(esi.FormatInt(prof.Losses))},
		}
		if st.IskKilled > 0 {
			s.Figures = append(s.Figures, card.Stat{Label: "ISK DESTROYED", Value: card.Figure(iskShortGo(st.IskKilled))})
		}
		if total := st.IskKilled + st.IskLost; total > 0 {
			s.Figures = append(s.Figures, card.Stat{Label: "ISK EFFICIENCY", Value: card.Figure(fmt.Sprintf("%.1f%%", st.IskKilled*100/total))})
		}
		if st.Solo > 0 {
			s.Minor = append(s.Minor, card.Stat{Label: "SOLO KILLS", Value: card.Figure(esi.FormatInt(st.Solo))})
		}
		if prof.FinalBlows > 0 {
			s.Minor = append(s.Minor, card.Stat{Label: "FINAL BLOWS", Value: card.Figure(esi.FormatInt(prof.FinalBlows))})
		}
		if prof.GangSamples > 0 {
			avg := float64(prof.GangTotal) / float64(prof.GangSamples)
			s.Minor = append(s.Minor, card.Stat{Label: "TYPICAL GANG", Value: card.Figure(strconv.FormatFloat(avg, 'f', 1, 64))})
		}
		if st.IskLost > 0 {
			s.Minor = append(s.Minor, card.Stat{Label: "ISK LOST", Value: card.Figure(iskShortGo(st.IskLost))})
		}
		names := app.newKillNames(ctx)
		if rows, err := app.queries.ListPilotActivityShips(ctx, db.ListPilotActivityShipsParams{CharacterID: id, MaxRows: pilotSheetShips}); err == nil && len(rows) > 0 {
			top := float64(rows[0].Flown + rows[0].Lost)
			for _, row := range rows {
				n := row.Flown + row.Lost
				s.Ships = append(s.Ships, card.ProfileRow{Name: names.typeName(row.ShipTypeID), Count: esi.FormatInt(n), Frac: float64(n) / top})
				shipIcons = append(shipIcons, fmt.Sprintf("https://images.evetech.net/types/%d/icon?size=64", row.ShipTypeID))
			}
		} else if err != nil {
			logging.Warnf("pilot sheet %d: ships: %v", id, err)
		}
		if rows, err := app.queries.ListPilotActivityHours(ctx, id); err == nil && len(rows) > 0 {
			hours := make([]pilotHour, 24)
			var top int64
			for h := range hours {
				hours[h].Hour = h
			}
			for _, row := range rows {
				if row.Hour >= 0 && row.Hour < 24 {
					hours[row.Hour].N = row.Events
					top = max(top, row.Events)
				}
			}
			if top > 0 {
				s.Hours = make([]float64, 24)
				for h := range hours {
					s.Hours[h] = float64(hours[h].N) / float64(top)
				}
				s.HoursNote = peakHours(hours)
			}
		} else if err != nil {
			logging.Warnf("pilot sheet %d: hours: %v", id, err)
		}
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		logging.Warnf("pilot sheet %d: profile: %v", id, err)
	}
	if len(s.Figures) == 0 {
		s.Figures = []card.Stat{{Label: "KILLS", Value: card.Figure("0")}, {Label: "LOSSES", Value: card.Figure("0")}}
	}

	portrait := portraitURL(id, 512)
	// The version covers the stored data and everything drawn; the
	// age chip moves on a birthday, so the date is part of it.
	ver := pilotSheetVersion(data+"|"+now.UTC().Format("2006-01-02"), portrait, shipIcons, s)
	return pilotSheetJob{key: "pilotsheet:" + strconv.FormatInt(id, 10), version: ver,
		sheet: s, portrait: portrait, shipIcons: shipIcons}, true
}

// pilotSheetVersion hashes a sheet's data version with what it
// draws, by value, through the summary cards' version hash.
func pilotSheetVersion(data, portrait string, icons []string, s card.Profile) string {
	rows := make([]card.Stat, 0, len(s.Ships)+len(s.Lines)+len(s.Facts)+1)
	for _, r := range s.Ships {
		rows = append(rows, card.Stat{Label: r.Name + "|" + r.Count + "|" + strconv.FormatFloat(r.Frac, 'f', 4, 64)})
	}
	for _, l := range s.Lines {
		rows = append(rows, card.Stat{Label: l})
	}
	for _, f := range s.Facts {
		rows = append(rows, card.Stat{Label: f.Label + "=" + f.Text})
	}
	hours := make([]string, len(s.Hours))
	for i, h := range s.Hours {
		hours[i] = strconv.FormatFloat(h, 'f', 4, 64)
	}
	rows = append(rows, card.Stat{Label: strings.Join(hours, ",") + "|" + s.HoursNote})
	stats := append(append(append([]card.Stat{}, s.Chips...), s.Figures...), s.Minor...)
	stats = append(stats, rows...)
	return cardVersion("sheet|"+data+"|"+strings.Join(icons, ","), portrait, card.Card{Kind: s.Kind, Title: s.Title, Stats: stats})
}

// pilotSecColor colours a pilot's security status the way the game
// reads it: deep negative is an outlaw's red, mildly negative a
// warning orange, positive the high-sec blue.
func pilotSecColor(sec float64) (c color.NRGBA) {
	switch {
	case sec <= -5:
		return card.SecColor(1)
	case sec < 0:
		return card.SecColor(4)
	case sec >= 0.5:
		return card.SecColor(9)
	}
	return c
}

// pilotShareView is the character sheet's Share pilot card dialog:
// the large sheet, its download, and the public profile as the link.
func pilotShareView(id int64, name string) *module.ShareCard {
	q := strconv.FormatInt(id, 10)
	return &module.ShareCard{
		ID:       "pilot-share",
		Title:    "Share pilot card",
		Lead:     "Your public record and killboard figures. Nothing private: no wallet, location, skills or assets.",
		Image:    "/pilot/share.png?character=" + q,
		Download: "/pilot/share.png?character=" + q + "&download=1",
		Width:    card.ProfileW, Height: card.ProfileH,
		Alt:      "Pilot card for " + name,
		Link:     "/pilot/?character=" + q,
		FileName: name + " - EveSynapse.png",
		Note:     "The link opens your public profile; chat apps show its summary card.",
	}
}
