package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Contacts page (/contacts/): the character's contact list with
// standings, cache-only from the worker-warmed contacts snapshot
// (every page merged). Read-only: editing contacts needs
// esi-characters.write_contacts.v1, which the app never requested.
// Names resolve per contact kind from the local caches; factions
// resolve from the warmed global factions snapshot.
// ---------------------------------------------------------------------------

type contactRow struct {
	Name     string
	ID       int64
	IsChar   bool   // contact is a character (others stay text)
	Type     string // display-cased contact kind
	Standing string // signed, one decimal
	Watched  bool
	Blocked  bool
}

type contactsView struct {
	CharacterName string
	Contacts      econSectionState
	Rows          []contactRow
}

func (app *Application) handleContacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	_, active, links, err := app.pickCharacter(ctx, r, "/contacts/")
	if err != nil {
		log.Printf("contacts: list characters: %v", err)
		data.Error = "Could not load contacts; check the server log."
		app.render(ctx, w, http.StatusOK, "contacts.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "contacts.html", data)
		return
	}
	data.ContactsChars = links

	view := &contactsView{CharacterName: active.Name}
	data.Contacts = view

	var contacts esi.Contacts
	view.Contacts = app.econSection(ctx, active.CharacterID, esi.SnapContacts, &contacts)
	if view.Contacts.Loaded {
		type pendingRow struct {
			row      contactRow
			standing float64
		}
		pending := make([]pendingRow, 0, len(contacts))
		for _, c := range contacts {
			pending = append(pending, pendingRow{
				row: contactRow{
					Name:     app.contactDisplayName(ctx, c),
					ID:       c.ContactID,
					IsChar:   c.ContactType == "character",
					Type:     humanizeEnum(c.ContactType),
					Standing: fmt.Sprintf("%+.1f", c.Standing),
					Watched:  c.IsWatched,
					Blocked:  c.IsBlocked,
				},
				standing: c.Standing,
			})
		}
		// Standing first (excellent → terrible), then name:
		// the order a pilot triages a contact list in.
		sort.SliceStable(pending, func(i, j int) bool {
			if pending[i].standing != pending[j].standing {
				return pending[i].standing > pending[j].standing
			}
			return pending[i].row.Name < pending[j].row.Name
		})
		for _, p := range pending {
			view.Rows = append(view.Rows, p.row)
		}
	}

	app.render(ctx, w, http.StatusOK, "contacts.html", data)
}

// contactDisplayName resolves a contact's name by kind from the
// local caches, with honest id fallbacks.
func (app *Application) contactDisplayName(ctx context.Context, c esi.Contact) string {
	switch c.ContactType {
	case "character":
		return characterDisplay(app.esi, c.ContactID)
	case "corporation":
		if name, ok := app.esi.CachedCorpName(c.ContactID); ok && name != "" {
			return name
		}
		return fmt.Sprintf("Corporation #%d", c.ContactID)
	case "alliance":
		if name, ok := app.esi.CachedAllianceName(c.ContactID); ok && name != "" {
			return name
		}
		return fmt.Sprintf("Alliance #%d", c.ContactID)
	case "faction":
		if name := app.factionName(ctx, c.ContactID); name != "" {
			return name
		}
		return fmt.Sprintf("Faction #%d", c.ContactID)
	default:
		return fmt.Sprintf("#%d", c.ContactID)
	}
}

// factionName resolves an NPC faction id from the warmed global
// factions snapshot (intel cluster), "" when absent.
func (app *Application) factionName(ctx context.Context, factionID int64) string {
	var factions esi.Factions
	if !app.loadGlobalSnapshot(ctx, esi.GlobalFactions, &factions) {
		return ""
	}
	for _, f := range factions {
		if f.FactionID == factionID {
			return f.Name
		}
	}
	return ""
}
