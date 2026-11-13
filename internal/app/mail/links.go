package mail

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"strconv"
	"strings"

	"evesynapse/internal/htmlsanitize"
)

// EVE writes links into mail bodies in its own schemes. These turn
// them into the EveSynapse pages that show the same thing, so a
// system, pilot, corporation, alliance, item or kill in a mail is one
// click from its page, and a fitting is one click from the fitting
// tool.

// mailFit is a fitting linked in a mail, ready for the fitting tool.
type mailFit struct {
	Ship   string
	ShipID int64
	EFT    string
}

// gameLinkPath maps one EVE link to an EveSynapse path
// (htmlsanitize.EVELinkPath), and a fitting link to its hull's page.
// fits collects the fittings it meets (fitting:<ship>:<type>;<count>:...).
func (app *mod) gameLinkPath(ctx context.Context, href string, fits *[]mailFit) (string, bool) {
	if strings.HasPrefix(href, "fitting:") {
		if f, ok := app.fitFromDNA(ctx, strings.TrimPrefix(href, "fitting:")); ok {
			*fits = append(*fits, f)
			return fmt.Sprintf("/items/type/%d/", f.ShipID), true
		}
		return "", false
	}
	return htmlsanitize.EVELinkPath(href)
}

// fitFromDNA turns EVE's fitting string (ship type, then
// type;count pairs) into EFT text the fitting tool imports.
func (app *mod) fitFromDNA(ctx context.Context, dna string) (mailFit, bool) {
	parts := strings.Split(strings.Trim(dna, ":"), ":")
	if len(parts) == 0 {
		return mailFit{}, false
	}
	shipID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || shipID <= 0 {
		return mailFit{}, false
	}
	ship := app.deps.TypeName(ctx, shipID)
	var b strings.Builder
	fmt.Fprintf(&b, "[%s, Fit from mail]\n", ship)
	for _, p := range parts[1:] {
		idText, countText, _ := strings.Cut(p, ";")
		id, err := strconv.ParseInt(strings.TrimSuffix(idText, "_"), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		count, _ := strconv.Atoi(countText)
		if count <= 0 {
			count = 1
		}
		name := app.deps.TypeName(ctx, id)
		if count == 1 {
			b.WriteString(name + "\n")
		} else {
			fmt.Fprintf(&b, "%s x%d\n", name, count)
		}
	}
	return mailFit{Ship: ship, ShipID: shipID, EFT: b.String()}, true
}

// replyHref and forwardHref open the compose page filled in for an
// answer to, or a copy of, the open mail; back returns to it after
// sending.
func replyHref(charID int64, d *mailDetail, back string) string {
	v := url.Values{}
	v.Set("character", strconv.FormatInt(charID, 10))
	v.Set("to", d.From)
	v.Set("subject", prefixSubject("Re: ", d.Subject))
	v.Set("back", back)
	return "/mail/compose/?" + v.Encode()
}

func forwardHref(charID int64, d *mailDetail, plain, back string) string {
	v := url.Values{}
	v.Set("character", strconv.FormatInt(charID, 10))
	v.Set("subject", prefixSubject("Fwd: ", d.Subject))
	v.Set("body", "\n\n-------- Forwarded mail --------\nFrom: "+d.From+"\nDate: "+d.Date+"\nSubject: "+d.Subject+"\n\n"+plain)
	v.Set("back", back)
	return "/mail/compose/?" + v.Encode()
}

func prefixSubject(prefix, subject string) string {
	if strings.HasPrefix(strings.ToLower(subject), strings.ToLower(prefix)) {
		return subject
	}
	return prefix + subject
}

// plainBody is a mail body as text, for forwarding: tags dropped,
// line breaks kept.
func plainBody(raw string) string {
	r := strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n")
	s := r.Replace(raw)
	var b strings.Builder
	in := false
	for _, c := range s {
		switch {
		case c == '<':
			in = true
		case c == '>':
			in = false
		case !in:
			b.WriteRune(c)
		}
	}
	return strings.TrimSpace(html.UnescapeString(b.String()))
}

// safeBack keeps a return path on the mail pages only.
func safeBack(back string) string {
	if strings.HasPrefix(back, "/mail/") && !strings.HasPrefix(back, "//") {
		return back
	}
	return ""
}
