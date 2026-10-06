package app

import (
	"context"
	"errors"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Mail page (/mail/): the character's 50 most recent headers, label
// filter, and a body view — all cache-only from the worker-warmed
// mail snapshots. READ-ONLY by construction: the app requests
// esi-mail.read_mail.v1 only (organize/send were never requested,
// see auth.go), so there is nothing to send or delete with.
//
// Bodies are EVE-flavored HTML and hostile by default: they are
// rendered only through sanitizeMailHTML, a strict allowlist
// filter, and external images are dropped with every other
// non-allowlisted tag.
// ---------------------------------------------------------------------------

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
	Labels  []string
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
	Body    template.HTML // sanitized; never raw ESI HTML
}

type mailView struct {
	CharacterID   int64
	CharacterName string
	Headers       econSectionState
	Rows          []mailRow
	UnreadCount   int64
	UnreadKnown   bool
	LabelChips    []mailLabelChip
	LabelFilter   int64 // 0 = all mail
	Selected      bool  // a mail is open below the list
	Detail        *mailDetail
	DetailID      int64 // open mail's ID, for the mark-as-read form
	DetailWarming bool  // body snapshot has not warmed yet
}

func (app *Application) handleMail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	_, active, links, err := app.pickCharacter(ctx, r, "/mail/")
	if err != nil {
		log.Printf("mail: list characters: %v", err)
		data.Error = "Could not load mail; check the server log."
		app.render(ctx, w, http.StatusOK, "mail.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "mail.html", data)
		return
	}
	data.MailChars = links

	view := &mailView{CharacterID: active.CharacterID, CharacterName: active.Name}
	data.Mail = view

	mailID, _ := strconv.ParseInt(r.URL.Query().Get("mail"), 10, 64)
	labelFilter, _ := strconv.ParseInt(r.URL.Query().Get("label"), 10, 64)
	view.LabelFilter = labelFilter

	// Label set: names for header chips + the total unread count.
	labelNames := make(map[int64]string)
	var labels esi.MailLabels
	if app.loadCorpSnapshot(ctx, active.CharacterID, esi.SnapMailLabels, &labels) {
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
	if app.loadCorpSnapshot(ctx, active.CharacterID, esi.SnapMailLists, &lists) {
		for _, l := range lists {
			listNames[l.MailingListID] = l.Name
		}
	}

	var headers esi.MailHeaders
	view.Headers = app.econSection(ctx, active.CharacterID, esi.SnapMail, &headers)
	if view.Headers.Loaded {
		for _, h := range headers {
			if labelFilter != 0 && !containsInt64(h.Labels, labelFilter) {
				continue
			}
			row := mailRow{
				ID:      h.MailID,
				Subject: h.Subject,
				From:    app.displayCharacter(ctx, h.From),
				FromID:  h.From,
				Date:    formatFinish(h.Timestamp),
				Unread:  !h.IsRead,
			}
			for _, id := range h.Labels {
				if name, ok := labelNames[id]; ok {
					row.Labels = append(row.Labels, name)
				}
			}
			view.Rows = append(view.Rows, row)
		}
	}

	// An open mail renders under the list, from its body snapshot.
	if mailID > 0 {
		view.Selected = true
		view.DetailID = mailID
		var mail esi.Mail
		if app.loadCorpSnapshot(ctx, active.CharacterID, esi.MailBodyKind(mailID), &mail) {
			detail := &mailDetail{
				Subject: mail.Subject,
				From:    app.displayCharacter(ctx, mail.From),
				FromID:  mail.From,
				Date:    formatFinish(mail.Timestamp),
				Body:    sanitizeMailHTML(mail.Body),
			}
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

	app.render(ctx, w, http.StatusOK, "mail.html", data)
}

// mailRecipientDisplay renders one mail recipient by kind:
// character, corporation, alliance and mailing-list names from
// the local caches, with honest id fallbacks.
func (app *Application) mailRecipientDisplay(ctx context.Context, rcpt esi.MailRecipient, listNames map[int64]string) mailRecipientView {
	out := mailRecipientView{Kind: rcpt.RecipientType, ID: rcpt.RecipientID}
	switch rcpt.RecipientType {
	case "character":
		out.Name = app.displayCharacter(ctx, rcpt.RecipientID)
	case "corporation":
		out.Name = app.corpDisplayName(ctx, rcpt.RecipientID)
	case "alliance":
		out.Name = app.allianceDisplayName(ctx, rcpt.RecipientID)
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

func containsInt64(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Mail body sanitizer. EVE mail bodies are HTML written by other
// players: hostile input. The filter below keeps a small allowlist
// of formatting tags, drops active-content tags together with
// their contents, keeps only http(s) links, drops every other
// attribute, and escapes all text. Nothing from the source reaches
// the page unescaped except the literal tags emitted here.
// ---------------------------------------------------------------------------

// mailAllowedTags survive sanitizing (attributes stripped; <a>
// keeps a validated href only).
var mailAllowedTags = map[string]bool{
	"a": true, "b": true, "strong": true, "i": true, "em": true,
	"u": true, "s": true, "strike": true, "sub": true, "sup": true,
	"p": true, "br": true, "hr": true, "div": true, "span": true,
	"font": true, "ul": true, "ol": true, "li": true,
	"blockquote": true, "pre": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true,
	"tr": true, "td": true, "th": true, "caption": true,
}

// mailDropWithContents tags vanish together with everything up to
// their matching close tag (active or embedding content —
// including images, which are also external beacons).
var mailDropWithContents = map[string]bool{
	"script": true, "style": true, "iframe": true, "object": true,
	"embed": true, "form": true, "textarea": true, "select": true,
	"button": true, "title": true, "head": true, "frameset": true,
	"frame": true, "applet": true, "svg": true, "math": true,
	"video": true, "audio": true, "canvas": true, "noscript": true,
	"template": true, "img": true, "input": true, "link": true,
	"meta": true, "base": true, "source": true, "track": true,
}

// sanitizeMailHTML reduces a raw mail body to safe display HTML.
func sanitizeMailHTML(raw string) template.HTML {
	var out strings.Builder
	// droppedAnchors counts <a> open tags swallowed for a bad
	// href, so their close tags are swallowed with them.
	droppedAnchors := 0
	i := 0
	for i < len(raw) {
		lt := strings.IndexByte(raw[i:], '<')
		if lt < 0 {
			writeMailText(&out, raw[i:])
			break
		}
		writeMailText(&out, raw[i:i+lt])
		i += lt

		rest := raw[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			// Comments are dropped whole; an unterminated
			// comment swallows the rest.
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				return template.HTML(out.String())
			}
			i += 4 + end + 3
			continue
		case strings.HasPrefix(rest, "<!"), strings.HasPrefix(rest, "<?"):
			// Doctypes / processing instructions: skip to '>'.
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				return template.HTML(out.String())
			}
			i += end + 1
			continue
		}

		// A tag: optional '/', then a name. Anything else makes
		// the '<' literal text.
		j := i + 1
		closing := false
		if j < len(raw) && raw[j] == '/' {
			closing = true
			j++
		}
		start := j
		for j < len(raw) && isMailTagNameChar(raw[j]) {
			j++
		}
		if j == start {
			writeMailText(&out, "<")
			i++
			continue
		}
		name := strings.ToLower(raw[start:j])

		end := scanMailTagEnd(raw, j)
		if end < 0 {
			// Unterminated tag: the '<' is literal text.
			writeMailText(&out, "<")
			i++
			continue
		}
		attrs := raw[j:end]
		selfClosing := strings.HasSuffix(strings.TrimSpace(attrs), "/")
		i = end + 1

		if closing {
			if name == "a" && droppedAnchors > 0 {
				droppedAnchors--
				continue
			}
			if mailAllowedTags[name] && name != "br" && name != "hr" {
				out.WriteString("</" + name + ">")
			}
			continue
		}
		if mailDropWithContents[name] {
			if selfClosing || name == "img" || name == "input" || name == "link" || name == "meta" || name == "base" || name == "source" || name == "track" || name == "frame" || name == "embed" {
				continue // void droppers have no contents to skip
			}
			closeIdx := indexMailCloseTag(raw, i, name)
			if closeIdx < 0 {
				return template.HTML(out.String()) // rest is swallowed content
			}
			gt := strings.IndexByte(raw[closeIdx:], '>')
			if gt < 0 {
				return template.HTML(out.String())
			}
			i = closeIdx + gt + 1
			continue
		}
		if !mailAllowedTags[name] {
			continue // unknown tag: drop the tag, keep its text
		}
		switch name {
		case "br":
			out.WriteString("<br>")
		case "hr":
			out.WriteString("<hr>")
		case "a":
			if href, ok := mailHref(attrs); ok {
				out.WriteString(`<a href="` + html.EscapeString(href) + `" rel="nofollow noopener noreferrer" target="_blank">`)
			} else {
				// An unusable href leaves the link text bare;
				// remember to swallow its close tag too.
				droppedAnchors++
			}
		default:
			out.WriteString("<" + name + ">")
		}
	}
	return template.HTML(out.String())
}

// writeMailText appends a text run: entities decoded, then
// re-escaped, so nothing textual reaches the page as markup.
func writeMailText(out *strings.Builder, s string) {
	if s == "" {
		return
	}
	out.WriteString(html.EscapeString(html.UnescapeString(s)))
}

// isMailTagNameChar reports whether c can appear in a tag name.
func isMailTagNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// scanMailTagEnd finds the '>' ending a tag starting search at
// from, skipping over quoted attribute values.
func scanMailTagEnd(s string, from int) int {
	var quote byte
	for i := from; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '>':
			return i
		}
	}
	return -1
}

// indexMailCloseTag locates the close tag for a
// drop-with-contents element, case-insensitively.
func indexMailCloseTag(s string, from int, name string) int {
	idx := strings.Index(strings.ToLower(s[from:]), "</"+name)
	if idx < 0 {
		return -1
	}
	return from + idx
}

// mailHref extracts and validates the href of an <a> tag's
// attribute text: only absolute http(s) URLs with a host survive
// (EVE's showinfo:/javascript:/relative links do not).
func mailHref(attrs string) (string, bool) {
	lower := strings.ToLower(attrs)
	for search := 0; search < len(lower); {
		idx := strings.Index(lower[search:], "href")
		if idx < 0 {
			return "", false
		}
		pos := search + idx
		// href must start an attribute (preceded by space).
		if pos > 0 && lower[pos-1] != ' ' && lower[pos-1] != '\t' {
			search = pos + 4
			continue
		}
		rest := attrs[pos+4:]
		rest = strings.TrimLeft(rest, " \t")
		if !strings.HasPrefix(rest, "=") {
			search = pos + 4
			continue
		}
		rest = strings.TrimLeft(rest[1:], " \t")
		var value string
		if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'') {
			quote := rest[0]
			if end := strings.IndexByte(rest[1:], quote); end >= 0 {
				value = rest[1 : 1+end]
			}
		} else if end := strings.IndexAny(rest, " \t"); end >= 0 {
			value = rest[:end]
		} else {
			value = rest
		}
		value = html.UnescapeString(strings.TrimSpace(value))
		u, err := url.Parse(value)
		if err != nil || u.Host == "" {
			return "", false
		}
		if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
			return "", false
		}
		return value, true
	}
	return "", false
}

// handleMailMarkRead serves POST /mail/read/ (Issue 26): mark one
// mail read in-game via PUT /characters/{id}/mail/{mail_id}/,
// which needs the esi-mail.organize_mail.v1 scope. A 403 means
// the character was linked before that scope existed — reported
// as "sign in again", never silently swallowed.
func (app *Application) handleMailMarkRead(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
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
	token, err := app.validAccessToken(ctx, ch)
	if err != nil {
		log.Printf("mail mark-read: token for character %d: %v", charID, err)
		http.Error(w, "Could not reach EVE. Sign in again if it keeps failing.", http.StatusBadGateway)
		return
	}
	path := fmt.Sprintf("/characters/%d/mail/%d/", charID, mailID)
	if err := app.esi.PutJSONAuthed(ctx, token, path, map[string]bool{"read": true}); err != nil {
		var se *esi.StatusError
		if errors.As(err, &se) && se.Code == http.StatusForbidden {
			http.Error(w, ch.Name+" was linked before EveSynapse asked for mail organize access — sign in again to grant it.", http.StatusForbidden)
			return
		}
		log.Printf("mail mark-read: ESI PUT %s: %v", path, err)
		http.Error(w, "EVE refused the update.", http.StatusBadGateway)
		return
	}
	// Success: back to the mail view.
	http.Redirect(w, r, fmt.Sprintf("/mail/?character=%d&mail=%d", charID, mailID), http.StatusSeeOther)
}
