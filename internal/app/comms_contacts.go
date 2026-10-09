package app

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
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
	Name        string
	ID          int64
	IsChar      bool   // contact is a character (others stay text)
	Kind        string // raw contact kind: character | corporation | alliance | faction
	NamePending bool   // character name still on its way; the row polls for it
	PollURL     string // live-region fragment for a pending character name
	Type        string // display-cased contact kind
	Standing    string // signed, one decimal
	Watched     bool
	Blocked     bool
}

type contactsView struct {
	CharacterName string
	Contacts      econSectionState
	Rows          []contactRow
}

func (app *Application) handleContacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	_, active, links, err := app.pickCharacter(ctx, r, "/contacts/")
	if err != nil {
		logging.Errorf("contacts: list characters: %v", err)
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
			row := contactRow{
				ID:       c.ContactID,
				IsChar:   c.ContactType == "character",
				Kind:     c.ContactType,
				Type:     humanizeEnum(c.ContactType),
				Standing: fmt.Sprintf("%+.1f", c.Standing),
				Watched:  c.IsWatched,
				Blocked:  c.IsBlocked,
			}
			if row.IsChar {
				// A contact's name is current-page data: an
				// unresolved one leaves a viewed-priority want
				// and renders as a live region that swaps the
				// resolved, linked name in without a refresh.
				if name, settled := app.resolvedCharacterName(ctx, c.ContactID); settled && name != "" {
					row.Name = name
				} else if settled {
					row.Name = fmt.Sprintf("Character #%d", c.ContactID)
				} else {
					app.notePageWant(ctx, pageWantCharacter, c.ContactID, 0)
					row.Name = fmt.Sprintf("Character #%d", c.ContactID)
					row.NamePending = true
					row.PollURL = fmt.Sprintf("/contacts/name-fragment?character=%d&contact=%d", active.CharacterID, c.ContactID)
				}
			} else {
				row.Name = app.contactDisplayName(ctx, c)
			}
			pending = append(pending, pendingRow{row: row, standing: c.Standing})
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

// handleContactNameFragment re-renders one contact's name cell
// from the local caches: the resolved name as a pilot link once
// it lands, a settled plain fallback when ESI has no such
// character, and the pending live region until then. Cache-only —
// the contacts page already left the want; this read never
// enqueues and never fetches.
func (app *Application) handleContactNameFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	ownerID, oerr := strconv.ParseInt(q.Get("character"), 10, 64)
	contactID, cerr := strconv.ParseInt(q.Get("contact"), 10, 64)
	if oerr != nil || cerr != nil || ownerID <= 0 || contactID <= 0 {
		http.Error(w, "bad contact name fragment request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	name, settled := app.resolvedCharacterName(ctx, contactID)
	switch {
	case settled && name != "":
		fmt.Fprintf(w, `<span data-poll-state="ready">%s</span>`, charLink(app.viewerCharSet(ctx), contactID, name))
	case settled:
		fmt.Fprintf(w, `<span data-poll-state="ready">Character #%d</span>`, contactID)
	default:
		fmt.Fprintf(w, `<span data-poll-state="pending"><span class="loading-pulse" aria-hidden="true"></span> Loading name for Character #%d…</span>`, contactID)
	}
}

// contactDisplayName resolves a contact's name by kind from the
// local caches, with id fallbacks.
func (app *Application) contactDisplayName(ctx context.Context, c esi.Contact) string {
	switch c.ContactType {
	case "character":
		return app.displayCharacter(ctx, c.ContactID)
	case "corporation":
		return app.corpDisplayName(ctx, c.ContactID)
	case "alliance":
		return app.allianceDisplayName(ctx, c.ContactID)
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
