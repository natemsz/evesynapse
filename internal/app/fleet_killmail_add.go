package app

// ---------------------------------------------------------------------------
// Adding a missing killmail to a fleet's report. zKillboard now and then
// loses a kill while it processes it, and a pilot with no account here
// (or without the killmail scope) brings in none of their own, so a kill
// can be missing from both. A pilot who can open the report pastes the
// kill's link and the board fetches it, in this order: the copy it
// already holds; CCP's ESI, by the id and hash in the external kill link
// the game copies; then zKillboard, for a zKillboard link, which names
// only the id. A kill fetched this way is stored like any other and is
// public: the pilot posted it. The fleet's report is queued to rebuild,
// and it takes the kill in if the kill belongs to the fleet.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/logging"
)

const (
	fleetAddMailsMax   = 10 // killmails read from one post
	fleetAddMailsZKill = 3  // zKillboard lookups one post may make, at its pace
)

// killmailRef is one pasted killmail: its id, and its hash when the
// link carried one.
type killmailRef struct {
	ID   int64
	Hash string
}

var (
	esiKillLink   = regexp.MustCompile(`killmails/(\d+)/([0-9a-fA-F]{40})`)
	zkillKillLink = regexp.MustCompile(`zkillboard\.com/kill/(\d+)`)
	boardKillLink = regexp.MustCompile(`/intel/kill/(\d+)`)
	bareKillID    = regexp.MustCompile(`^\d{5,12}$`)
)

// parseKillmailLinks reads the killmails named in pasted text: external
// kill links, zKillboard links, this killboard's links, or bare ids,
// separated by lines or spaces. Each kill counts once, and at most
// fleetAddMailsMax are read. bad counts the pieces that named none.
func parseKillmailLinks(text string) (refs []killmailRef, bad int) {
	seen := map[int64]bool{}
	for _, piece := range strings.Fields(text) {
		piece = strings.Trim(piece, ",;<>\"'")
		var ref killmailRef
		if m := esiKillLink.FindStringSubmatch(piece); m != nil {
			ref.ID, _ = strconv.ParseInt(m[1], 10, 64)
			ref.Hash = strings.ToLower(m[2])
		} else if m := zkillKillLink.FindStringSubmatch(piece); m != nil {
			ref.ID, _ = strconv.ParseInt(m[1], 10, 64)
		} else if m := boardKillLink.FindStringSubmatch(piece); m != nil {
			ref.ID, _ = strconv.ParseInt(m[1], 10, 64)
		} else if bareKillID.MatchString(piece) {
			ref.ID, _ = strconv.ParseInt(piece, 10, 64)
		}
		if ref.ID <= 0 {
			bad++
			continue
		}
		if seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true
		if len(refs) == fleetAddMailsMax {
			continue
		}
		refs = append(refs, ref)
	}
	return refs, bad
}

// killmailFound says where a pasted kill came from, or why it did not.
type killmailFound int

const (
	killmailHeld       killmailFound = iota // already on the board
	killmailFromESI                         // fetched from CCP by its hash
	killmailFromZKill                       // fetched from zKillboard
	killmailNotOnESI                        // CCP did not return it: a wrong hash
	killmailNotOnZKill                      // zKillboard does not have it
	killmailLater                           // over this post's zKillboard lookups
	killmailFailed                          // a fetch or the store failed
)

// fetchMissingKillmail finds one pasted kill: the board's own copy,
// then CCP's ESI by its hash, then zKillboard. zkillLeft counts down the
// zKillboard lookups this post may still make.
func (app *Application) fetchMissingKillmail(ctx context.Context, ref killmailRef, client *http.Client, zkillLeft *int) killmailFound {
	if _, err := app.queries.GetKill(ctx, ref.ID); err == nil {
		return killmailHeld
	}
	fromESI := func(hash string) killmailFound {
		body, _, err := app.esi.FetchRawOnce(ctx, fmt.Sprintf("/killmails/%d/%s/", ref.ID, hash))
		if err != nil {
			logging.Infof("fleets: added killmail %d not on ESI: %v", ref.ID, err)
			return killmailNotOnESI
		}
		if _, err := app.ingestKill(ctx, body, hash, nil, "esi", true, true); err != nil {
			logging.Errorf("fleets: store added killmail %d: %v", ref.ID, err)
			return killmailFailed
		}
		return killmailFromESI
	}
	if ref.Hash != "" {
		return fromESI(ref.Hash)
	}
	if *zkillLeft <= 0 {
		return killmailLater
	}
	if *zkillLeft < fleetAddMailsZKill && !killfeedSleepCtx(ctx, pilotBackfillPace) {
		return killmailFailed
	}
	*zkillLeft--
	entries, err := zkillHistoryPage(ctx, client, app.cfg.ZKillAPIBaseURL(), zkillHistoryKill, ref.ID, 1)
	if err != nil {
		logging.Infof("fleets: added killmail %d from zKillboard: %v", ref.ID, err)
		return killmailFailed
	}
	for _, e := range entries {
		if e.KillmailID != ref.ID {
			continue
		}
		// zKillboard named the hash but sent no killmail: CCP has it.
		if e.KillmailTime.IsZero() && e.ZKB.Hash != "" {
			if found := fromESI(e.ZKB.Hash); found == killmailFromESI {
				return killmailFromZKill
			}
			return killmailFailed
		}
		if _, err := app.ingestKill(ctx, e.Raw, e.ZKB.Hash, &e.ZKB, "zkill", true, true); err != nil {
			logging.Errorf("fleets: store added killmail %d: %v", ref.ID, err)
			return killmailFailed
		}
		return killmailFromZKill
	}
	return killmailNotOnZKill
}

// handleFleetReportKillmails is POST /fleets/{opID}/report/killmails:
// adds the killmails a pilot pastes and queues the report to rebuild.
// It answers to who may open the Report tab, signed in.
func (app *Application) handleFleetReportKillmails(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	id, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	op, err := app.queries.GetOp(ctx, id)
	if err != nil || v.UserID == 0 || !app.fleetReportTab(ctx, op, v, time.Now()) {
		app.renderFleetHidden(w, r, v)
		return
	}
	back := fleetReportPath(op.ID) + "#fl-x-mails"
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	refs, bad := parseKillmailLinks(r.PostFormValue("links"))
	if len(refs) == 0 {
		app.flash(ctx, "No killmail link found. In game, open the killmail and choose Copy External Kill Link, then paste it here.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	client := &http.Client{Timeout: 20 * time.Second}
	zkillLeft := fleetAddMailsZKill
	var added, held int
	var notESI, notZKill, later, failed []string
	for _, ref := range refs {
		switch app.fetchMissingKillmail(ctx, ref, client, &zkillLeft) {
		case killmailHeld:
			held++
		case killmailFromESI, killmailFromZKill:
			added++
		case killmailNotOnESI:
			notESI = append(notESI, strconv.FormatInt(ref.ID, 10))
		case killmailNotOnZKill:
			notZKill = append(notZKill, strconv.FormatInt(ref.ID, 10))
		case killmailLater:
			later = append(later, strconv.FormatInt(ref.ID, 10))
		default:
			failed = append(failed, strconv.FormatInt(ref.ID, 10))
		}
	}
	if added+held > 0 {
		if err := app.queries.MarkFleetReportDirty(ctx, op.ID); err != nil {
			logging.Errorf("fleets: mark report of %d: %v", op.ID, err)
		}
	}
	app.flash(ctx, fleetAddMailsMessage(added, held, bad, notESI, notZKill, later, failed))
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// fleetAddMailsMessage says what came of a post, in plain sentences.
func fleetAddMailsMessage(added, held, bad int, notESI, notZKill, later, failed []string) string {
	var parts []string
	switch added {
	case 0:
	case 1:
		parts = append(parts, "Added 1 killmail.")
	default:
		parts = append(parts, fmt.Sprintf("Added %d killmails.", added))
	}
	switch held {
	case 0:
	case 1:
		parts = append(parts, "1 was already on the killboard.")
	default:
		parts = append(parts, fmt.Sprintf("%d were already on the killboard.", held))
	}
	if added+held > 0 {
		parts = append(parts, "The report takes them in when it next updates, within a few minutes. A killmail none of this fleet's pilots are on, or from another time, stays out of it.")
	}
	if len(notESI) > 0 {
		parts = append(parts, "EVE did not return kill "+strings.Join(notESI, ", ")+". Copy the external kill link again: it must end in the kill's hash.")
	}
	if len(notZKill) > 0 {
		parts = append(parts, "zKillboard has no record of kill "+strings.Join(notZKill, ", ")+". Paste the external kill link from the game instead.")
	}
	if len(later) > 0 {
		parts = append(parts, fmt.Sprintf("Kill %s waits: zKillboard is asked about %d kills at a time. Paste external kill links from the game to add more at once.", strings.Join(later, ", "), fleetAddMailsZKill))
	}
	if len(failed) > 0 {
		parts = append(parts, "Kill "+strings.Join(failed, ", ")+" could not be fetched just now. Try again in a minute.")
	}
	if bad > 0 && len(parts) == 0 {
		parts = append(parts, "No killmail link found. In game, open the killmail and choose Copy External Kill Link, then paste it here.")
	}
	return strings.Join(parts, " ")
}
