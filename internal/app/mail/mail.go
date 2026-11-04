package mail

import (
	"context"
	"errors"
	"evesynapse/internal/app/module"
	"evesynapse/internal/htmlsanitize"
	"evesynapse/internal/humanize"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

type mailLabelChip struct {
	ID     int64
	Name   string
	Unread int64
}

type mailRow struct {
	ID      int64
	Subject string
	From    string
	FromID  int64
	Date    string
	Unread  bool
	Sent    bool                // the viewing character sent this mail
	To      []mailRecipientView // recipients, shown instead of From in the Sent view
	Labels  []string
	Stamp   string // RFC3339, for ordering the every-character list
	Owner   string // the character the mail belongs to (every-character list)
	OwnerID int64
}

// mailRecipientView is one mail recipient with its kind, so the
// template links each by the name policy (characters to their
// pages, corporations and alliances to theirs).
type mailRecipientView struct {
	Kind string // character | corporation | alliance | mailing_list
	ID   int64
	Name string
}

type mailDetail struct {
	Subject string
	From    string
	FromID  int64
	Date    string
	To      []mailRecipientView
	Labels  []string
	Body    template.HTML // sanitized; never raw ESI HTML, game links mapped to EveSynapse pages
	// Answering and fits:
	Fits         []mailFit // fittings linked in the body, for the fitting tool
	ReplyHref    string
	ForwardHref  string
	QuickSubject string
	Back         string // this mail's page, where a reply returns
	CanSend      bool   // the character may send mail, and did not write this one
}

type mailView struct {
	CharacterID   int64
	CharacterName string
	Headers       module.EconSectionState
	Rows          []mailRow
	UnreadCount   int64
	UnreadKnown   bool
	LabelChips    []mailLabelChip
	LabelFilter   int64  // 0 = all labels
	Box           string // inbox | sent | all — which side of the mailbox the list shows
	Selected      bool   // a mail is open below the list
	Detail        *mailDetail
	DetailID      int64 // open mail's ID, for the mark-as-read form
	DetailWarming bool  // body snapshot has not warmed yet
	CanMarkRead   bool  // the character granted mail-organize access (esi-mail.organize_mail.v1)
	JustSent      bool  // ?sent=1: a reply sent from this page just went out
	AllChars      bool  // ?all=1: every character's mail in one list
	MultiChar     bool  // the account has several characters (the switch shows)
	BoxesView     bool  // ?boxes=1: the reader asked for the mailbox list itself (a phone's top level)
}

func (app *mod) handleMail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	characters, active, links, err := app.deps.PickCharacter(ctx, r, "/mail/")
	if err != nil {
		logging.Errorf("mail: list characters: %v", err)
		data.Error = "Could not load mail; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "mail.html", data)
		return
	}
	if links == nil {
		app.deps.Render(ctx, w, http.StatusOK, "mail.html", data)
		return
	}
	data.MailChars = links

	view := &mailView{CharacterID: active.CharacterID, CharacterName: active.Name}
	data.Mail = view

	mailID, _ := strconv.ParseInt(r.URL.Query().Get("mail"), 10, 64)
	labelFilter, _ := strconv.ParseInt(r.URL.Query().Get("label"), 10, 64)
	view.LabelFilter = labelFilter

	// Box view: ESI has no sent folder, so direction is derived
	// per header below; the landing view is the inbox.
	box := r.URL.Query().Get("box")
	if box != "sent" && box != "all" {
		box = "inbox"
	}
	view.Box = box
	view.BoxesView = r.URL.Query().Get("boxes") == "1"
	view.JustSent = r.URL.Query().Get("sent") == "1"

	// Label set: names for header chips + the total unread count.
	labelNames := make(map[int64]string)
	var labels esi.MailLabels
	if app.deps.LoadCorpSnapshot(ctx, active.CharacterID, esi.SnapMailLabels, &labels) {
		view.UnreadCount = labels.TotalUnreadCount
		view.UnreadKnown = true
		for _, l := range labels.Labels {
			labelNames[l.LabelID] = l.Name
			view.LabelChips = append(view.LabelChips, mailLabelChip{ID: l.LabelID, Name: l.Name, Unread: l.UnreadCount})
		}
		sort.Slice(view.LabelChips, func(i, j int) bool { return view.LabelChips[i].Name < view.LabelChips[j].Name })
	}

	// Mailing-list names resolve body recipients.
	listNames := make(map[int64]string)
	var lists esi.MailLists
	if app.deps.LoadCorpSnapshot(ctx, active.CharacterID, esi.SnapMailLists, &lists) {
		for _, l := range lists {
			listNames[l.MailingListID] = l.Name
		}
	}

	view.AllChars = len(characters) > 1 && r.URL.Query().Get("all") == "1"
	view.MultiChar = len(characters) > 1
	if !view.AllChars {
		var headers esi.MailHeaders
		view.Headers = app.deps.EconSection(ctx, active.CharacterID, esi.SnapMail, &headers)
		if view.Headers.Loaded {
			view.Rows = app.mailRows(ctx, active, headers, box, labelFilter, labelNames, listNames)
		}
	} else {
		// Every character's mail in one list, newest first; labels
		// are each character's own, so the list filters by box only.
		view.LabelFilter = 0
		view.Headers = module.EconSectionState{Loaded: true}
		var total int64
		for _, ch := range characters {
			var headers esi.MailHeaders
			if st := app.deps.EconSection(ctx, ch.CharacterID, esi.SnapMail, &headers); !st.Loaded {
				continue
			}
			chLabels := make(map[int64]string)
			var ls esi.MailLabels
			if app.deps.LoadCorpSnapshot(ctx, ch.CharacterID, esi.SnapMailLabels, &ls) {
				total += ls.TotalUnreadCount
				for _, l := range ls.Labels {
					chLabels[l.LabelID] = l.Name
				}
			}
			for _, row := range app.mailRows(ctx, ch, headers, box, 0, chLabels, listNames) {
				row.Owner, row.OwnerID = ch.Name, ch.CharacterID
				view.Rows = append(view.Rows, row)
			}
		}
		view.UnreadCount, view.UnreadKnown = total, true
		sort.SliceStable(view.Rows, func(a, b int) bool { return view.Rows[a].Stamp > view.Rows[b].Stamp })
		if len(view.Rows) > maxAllRows {
			view.Rows = view.Rows[:maxAllRows]
		}
	}

	// An open mail renders under the list, from its body snapshot.
	if mailID > 0 {
		view.Selected = true
		view.DetailID = mailID
		view.CanMarkRead = module.CharacterHasScope(active, module.MailOrganizeScope)
		var mail esi.Mail
		if app.deps.LoadCorpSnapshot(ctx, active.CharacterID, esi.MailBodyKind(mailID), &mail) {
			detail := &mailDetail{
				Subject: mail.Subject,
				From:    app.deps.DisplayCharacter(ctx, mail.From),
				FromID:  mail.From,
				Date:    humanize.Finish(mail.Timestamp),
			}
			detail.Body = htmlsanitize.SanitizeLinked(mail.Body, func(href string) (string, bool) {
				return app.gameLinkPath(ctx, href, &detail.Fits)
			})
			bq := r.URL.Query()
			bq.Del("sent")
			back := r.URL.Path + "?" + bq.Encode()
			detail.ReplyHref = replyHref(active.CharacterID, detail, back)
			detail.ForwardHref = forwardHref(active.CharacterID, detail, plainBody(mail.Body), back)
			detail.QuickSubject = prefixSubject("Re: ", mail.Subject)
			detail.Back = back
			detail.CanSend = module.CharacterHasScope(active, module.MailSendScope) && mail.From != active.CharacterID
			for _, rcpt := range mail.Recipients {
				detail.To = append(detail.To, app.mailRecipientDisplay(ctx, rcpt, listNames))
			}
			for _, id := range mail.Labels {
				if name, ok := labelNames[id]; ok {
					detail.Labels = append(detail.Labels, name)
				}
			}
			view.Detail = detail
		} else {
			view.DetailWarming = true
		}
	}

	app.deps.Render(ctx, w, http.StatusOK, "mail.html", data)
}

// mailRecipientDisplay renders one mail recipient by kind:
// character, corporation, alliance and mailing-list names from
// the local caches, with id fallbacks.
func (app *mod) mailRecipientDisplay(ctx context.Context, rcpt esi.MailRecipient, listNames map[int64]string) mailRecipientView {
	out := mailRecipientView{Kind: rcpt.RecipientType, ID: rcpt.RecipientID}
	switch rcpt.RecipientType {
	case "character":
		out.Name = app.deps.DisplayCharacter(ctx, rcpt.RecipientID)
	case "corporation":
		out.Name = app.deps.CorpDisplayName(ctx, rcpt.RecipientID)
	case "alliance":
		out.Name = app.deps.AllianceDisplayName(ctx, rcpt.RecipientID)
	case "mailing_list":
		if name, ok := listNames[rcpt.RecipientID]; ok && name != "" {
			out.Name = name
		} else {
			out.Name = fmt.Sprintf("Mailing list #%d", rcpt.RecipientID)
		}
	default:
		out.Kind = ""
		out.Name = fmt.Sprintf("#%d", rcpt.RecipientID)
	}
	return out
}

// handleMailMarkRead serves POST /mail/read/: mark one
// mail read in-game via PUT /characters/{id}/mail/{mail_id}/,
// which needs the esi-mail.organize_mail.v1 scope. A 403 means
// the character was linked before that scope existed — reported
// as "sign in again", never silently swallowed.
func (app *mod) handleMailMarkRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	charID, _ := strconv.ParseInt(r.Form.Get("character"), 10, 64)
	mailID, _ := strconv.ParseInt(r.Form.Get("mail"), 10, 64)
	if charID <= 0 || mailID <= 0 {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	ch, err := app.queries.GetCharacter(ctx, charID)
	if err != nil || ch.UserID != userID {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if !module.CharacterHasScope(ch, module.MailOrganizeScope) {
		http.Error(w, module.RelinkNotice(ch.Name, "mark mail read"), http.StatusForbidden)
		return
	}
	token, err := app.deps.AccessToken(ctx, ch)
	if err != nil {
		logging.Errorf("mail mark-read: token for character %d: %v", charID, err)
		http.Error(w, "Could not reach EVE. Sign in again if it keeps failing.", http.StatusBadGateway)
		return
	}
	path := fmt.Sprintf("/characters/%d/mail/%d/", charID, mailID)
	if err := app.deps.PutESI(ctx, token, path, map[string]bool{"read": true}); err != nil {
		var se *esi.StatusError
		if errors.As(err, &se) && se.Code == http.StatusForbidden {
			http.Error(w, ch.Name+" was linked before EveSynapse asked for mail organize access. Sign in again to grant it.", http.StatusForbidden)
			return
		}
		logging.Errorf("mail mark-read: ESI PUT %s: %v", path, err)
		http.Error(w, "EVE refused the update"+app.deps.ESIRefusalDetail(err)+".", http.StatusBadGateway)
		return
	}
	// ESI took it. The page renders from stored copies of the mail
	// list and label counts, so record the change there too, or the
	// mail keeps showing as unread until the worker's next refresh.
	app.deps.MarkMailReadLocally(ctx, charID, mailID)
	// Success: back to the mail view, in the inbox box.
	http.Redirect(w, r, fmt.Sprintf("/mail/?character=%d&box=inbox&mail=%d", charID, mailID), http.StatusSeeOther)
}

// mailComposeView is the compose form's view model.
type mailComposeView struct {
	CharacterID   int64
	CharacterName string
	To            string // sticky recipient name on error
	Subject       string // sticky subject on error
	Body          string // sticky body on error
	Error         string // send failure, user-safe
	Sent          bool   // just sent: show confirmation
	NeedsRelink   bool   // the character never granted send-mail access; the form is replaced by a re-link prompt
	Back          string // a mail page to return to once sent (Reply, Forward, quick reply)
}

// handleMailCompose serves GET /mail/compose/: the compose form.
func (app *mod) handleMailCompose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}
	_, active, links, err := app.deps.PickCharacter(ctx, r, "/mail/compose/")
	if err != nil {
		logging.Errorf("mail compose: list characters: %v", err)
		data.Error = "Could not load characters; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "compose.html", data)
		return
	}
	if links == nil {
		app.deps.Render(ctx, w, http.StatusOK, "compose.html", data)
		return
	}
	data.MailChars = links
	q := r.URL.Query()
	data.MailCompose = &mailComposeView{
		CharacterID:   active.CharacterID,
		CharacterName: active.Name,
		NeedsRelink:   !module.CharacterHasScope(active, module.MailSendScope),
		To:            q.Get("to"),
		Subject:       q.Get("subject"),
		Body:          q.Get("body"),
		Back:          safeBack(q.Get("back")),
	}
	app.deps.Render(ctx, w, http.StatusOK, "compose.html", data)
}

// mailRecipientResolution is the /universe/ids/ subset we need to
// address mail: characters, corporations, alliances.
type mailRecipientResolution struct {
	Characters   []esi.UniverseIDEntry `json:"characters"`
	Corporations []esi.UniverseIDEntry `json:"corporations"`
	Alliances    []esi.UniverseIDEntry `json:"alliances"`
}

// handleMailSend serves POST /mail/send/: resolve the recipient
// name, then POST /characters/{id}/mail/. A 403 means the character
// was linked before the send_mail scope existed.
func (app *mod) handleMailSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	charID, _ := strconv.ParseInt(r.Form.Get("character"), 10, 64)
	toName := strings.TrimSpace(r.Form.Get("to"))
	subject := strings.TrimSpace(r.Form.Get("subject"))
	body := r.Form.Get("body")

	data := &PageData{ShellData: app.deps.PageShell(ctx)}
	_, active, links, err := app.deps.PickCharacter(ctx, r, "/mail/compose/")
	if err == nil && links != nil {
		data.MailChars = links
	}
	view := &mailComposeView{
		CharacterID: charID, To: toName, Subject: subject, Body: body,
		Back: safeBack(r.Form.Get("back")),
	}
	if active.CharacterID != 0 {
		view.CharacterName = active.Name
	}
	data.MailCompose = view
	fail := func(msg string) {
		view.Error = msg
		app.deps.Render(ctx, w, http.StatusOK, "compose.html", data)
	}

	ch, err := app.queries.GetCharacter(ctx, charID)
	if err != nil || ch.UserID != userID {
		fail("That character isn't one of yours.")
		return
	}
	if !module.CharacterHasScope(ch, module.MailSendScope) {
		view.NeedsRelink = true
		fail(module.RelinkNotice(ch.Name, "send mail"))
		return
	}
	if toName == "" {
		fail("Enter a recipient.")
		return
	}
	if subject == "" {
		fail("Enter a subject.")
		return
	}
	if strings.TrimSpace(body) == "" {
		fail("Enter a message body.")
		return
	}

	// Resolve the recipient name to an ID + type.
	var res mailRecipientResolution
	if err := app.deps.PostESIPublic(ctx, "/universe/ids/", []string{toName}, &res); err != nil {
		logging.Errorf("mail send: resolve %q: %v", toName, err)
		fail("Could not resolve recipient " + toName + ". Check the spelling.")
		return
	}
	var recipientID int64
	var recipientType string
	switch {
	case len(res.Characters) > 0:
		recipientID, recipientType = res.Characters[0].ID, "character"
	case len(res.Corporations) > 0:
		recipientID, recipientType = res.Corporations[0].ID, "corporation"
	case len(res.Alliances) > 0:
		recipientID, recipientType = res.Alliances[0].ID, "alliance"
	default:
		fail("No character, corporation, or alliance named " + toName + " found.")
		return
	}

	// approved_cost: EVE charges a small fee per recipient; 100k
	// ISK approved headroom covers any normal mail.
	if err := app.deps.SendEVEMail(ctx, ch, recipientID, recipientType, subject, body, 100000); err != nil {
		if errors.Is(err, module.ErrMailToken) {
			fail("Could not reach EVE. Sign in again if it keeps failing.")
			return
		}
		var se *esi.StatusError
		if errors.As(err, &se) && se.Code == http.StatusForbidden {
			fail(ch.Name + " was linked before EveSynapse asked for mail send access. Sign in again to grant it.")
			return
		}
		logging.Errorf("mail send: character %d: %v", charID, err)
		fail("EVE refused the mail" + app.deps.ESIRefusalDetail(err) + ". Check the recipient and try again.")
		return
	}
	// Sent from a mail (a reply, a forward, the quick reply): back to
	// that mail, which says so.
	if view.Back != "" {
		sep := "?"
		if strings.Contains(view.Back, "?") {
			sep = "&"
		}
		http.Redirect(w, r, view.Back+sep+"sent=1", http.StatusSeeOther)
		return
	}
	view.Sent = true
	view.To, view.Subject, view.Body = "", "", ""
	app.deps.Render(ctx, w, http.StatusOK, "compose.html", data)
}

// maxAllRows caps the every-character list.
const maxAllRows = 100

// mailRows shapes one character's mail headers for the list: the
// box (inbox, sent, all) and the label filter applied.
func (app *mod) mailRows(ctx context.Context, ch db.Character, headers esi.MailHeaders, box string, labelFilter int64, labelNames, listNames map[int64]string) []mailRow {
	var rows []mailRow
	for _, h := range headers {
		// A header whose sender is the viewing character is
		// his own copy of a mail he sent — list mail keeps
		// one, flagged unread — never incoming mail.
		sent := h.From == ch.CharacterID
		switch box {
		case "sent":
			if !sent {
				continue
			}
		case "all":
		default: // inbox
			if sent {
				continue
			}
		}
		if labelFilter != 0 && !slices.Contains(h.Labels, labelFilter) {
			continue
		}
		row := mailRow{
			ID:      h.MailID,
			Subject: h.Subject,
			From:    app.deps.DisplayCharacter(ctx, h.From),
			FromID:  h.From,
			Date:    humanize.Finish(h.Timestamp),
			Stamp:   h.Timestamp,
			Unread:  !h.IsRead && !sent,
			Sent:    sent,
		}
		if sent {
			for _, rcpt := range h.Recipients {
				row.To = append(row.To, app.mailRecipientDisplay(ctx, rcpt, listNames))
			}
		}
		for _, id := range h.Labels {
			if name, ok := labelNames[id]; ok {
				row.Labels = append(row.Labels, name)
			}
		}
		rows = append(rows, row)
	}
	return rows
}
