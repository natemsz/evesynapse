// Package htmlsanitize makes text written by other players safe to show
// inside a page: mail, pilot bios and corporation descriptions. The
// output is always well-formed on its own, so it can never close the
// page's own containers.
package htmlsanitize

import (
	"html"
	"html/template"
	"net/url"
	"strings"
)

// MailAllowedTags survive sanitizing (attributes stripped; <a>
// keeps a validated href only).
var MailAllowedTags = map[string]bool{
	"a": true, "b": true, "strong": true, "i": true, "em": true,
	"u": true, "s": true, "strike": true, "sub": true, "sup": true,
	"p": true, "br": true, "hr": true, "div": true, "span": true,
	"font": true, "ul": true, "ol": true, "li": true,
	"blockquote": true, "pre": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true,
	"tr": true, "td": true, "th": true, "caption": true,
}

// MailDropWithContents tags vanish together with everything up to
// their matching close tag (active or embedding content —
// including images, which are also external beacons).
var MailDropWithContents = map[string]bool{
	"script": true, "style": true, "iframe": true, "object": true,
	"embed": true, "form": true, "textarea": true, "select": true,
	"button": true, "title": true, "head": true, "frameset": true,
	"frame": true, "applet": true, "svg": true, "math": true,
	"video": true, "audio": true, "canvas": true, "noscript": true,
	"template": true, "img": true, "input": true, "link": true,
	"meta": true, "base": true, "source": true, "track": true,
}

// MailVoidTags are the allowed tags that never have contents or a
// close tag.
var MailVoidTags = map[string]bool{"br": true, "hr": true}

// MailMaxDepth caps how deeply the output nests. Real mail and
// bios never come close; a body built to nest thousands deep is
// flattened past this point instead of being passed to the browser.
const MailMaxDepth = 64

// Sanitize reduces a raw mail body to safe display HTML.
// It also serves the other player- and CCP-written HTML the app
// shows: pilot bios, corporation descriptions, item descriptions.
//
// The output is always well-formed on its own. Every tag written is
// closed again — by its own close tag, by the close of an element
// around it, or at the end — and a close tag with nothing open to
// match is dropped. A hostile body therefore cannot close the
// page's own containers and draw outside the box it is shown in.
func Sanitize(raw string) template.HTML {
	var out strings.Builder

	// open is the stack of elements currently open in the output.
	// An entry that was not written (an <a> whose href was unusable,
	// or anything nested past MailMaxDepth) still takes its place,
	// so that its close tag is swallowed with it.
	type openTag struct {
		name    string
		written bool
	}
	var open []openTag
	openTagAs := func(name, markup string, write bool) {
		if len(open) >= MailMaxDepth {
			write = false
		}
		if write {
			out.WriteString(markup)
		}
		open = append(open, openTag{name: name, written: write})
	}
	closeDownTo := func(depth int) {
		for len(open) > depth {
			top := open[len(open)-1]
			open = open[:len(open)-1]
			if top.written {
				out.WriteString("</" + top.name + ">")
			}
		}
	}
	// closeTag closes the nearest open element of that name, and
	// with it anything left open inside it. No such element: the
	// close tag is a stray and is dropped.
	closeTag := func(name string) {
		for depth := len(open) - 1; depth >= 0; depth-- {
			if open[depth].name == name {
				closeDownTo(depth)
				return
			}
		}
	}
	finish := func() template.HTML {
		closeDownTo(0)
		return template.HTML(out.String())
	}

	i := 0
	for i < len(raw) {
		lt := strings.IndexByte(raw[i:], '<')
		if lt < 0 {
			WriteMailText(&out, raw[i:])
			break
		}
		WriteMailText(&out, raw[i:i+lt])
		i += lt

		rest := raw[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			// Comments are dropped whole; an unterminated
			// comment swallows the rest.
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				return finish()
			}
			i += 4 + end + 3
			continue
		case strings.HasPrefix(rest, "<!"), strings.HasPrefix(rest, "<?"):
			// Doctypes / processing instructions: skip to '>'.
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				return finish()
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
		for j < len(raw) && IsMailTagNameChar(raw[j]) {
			j++
		}
		if j == start {
			WriteMailText(&out, "<")
			i++
			continue
		}
		name := strings.ToLower(raw[start:j])

		end := ScanMailTagEnd(raw, j)
		if end < 0 {
			// Unterminated tag: the '<' is literal text.
			WriteMailText(&out, "<")
			i++
			continue
		}
		attrs := raw[j:end]
		selfClosing := strings.HasSuffix(strings.TrimSpace(attrs), "/")
		i = end + 1

		if closing {
			if MailAllowedTags[name] && !MailVoidTags[name] {
				closeTag(name)
			}
			continue
		}
		if MailDropWithContents[name] {
			if selfClosing || name == "img" || name == "input" || name == "link" || name == "meta" || name == "base" || name == "source" || name == "track" || name == "frame" || name == "embed" {
				continue // void droppers have no contents to skip
			}
			closeIdx := IndexMailCloseTag(raw, i, name)
			if closeIdx < 0 {
				return finish() // rest is swallowed content
			}
			gt := strings.IndexByte(raw[closeIdx:], '>')
			if gt < 0 {
				return finish()
			}
			i = closeIdx + gt + 1
			continue
		}
		if !MailAllowedTags[name] {
			continue // unknown tag: drop the tag, keep its text
		}
		switch {
		case MailVoidTags[name]:
			out.WriteString("<" + name + ">")
		case name == "a":
			// An unusable href leaves the link text bare; the
			// unwritten entry swallows its close tag too.
			href, ok := MailHref(attrs)
			openTagAs("a", `<a href="`+html.EscapeString(href)+`" rel="nofollow noopener noreferrer" target="_blank">`, ok)
		default:
			openTagAs(name, "<"+name+">", true)
		}
	}
	return finish()
}

// WriteMailText appends a text run: entities decoded, then
// re-escaped, so nothing textual reaches the page as markup.
func WriteMailText(out *strings.Builder, s string) {
	if s == "" {
		return
	}
	out.WriteString(html.EscapeString(html.UnescapeString(s)))
}

// IsMailTagNameChar reports whether c can appear in a tag name.
func IsMailTagNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// ScanMailTagEnd finds the '>' ending a tag starting search at
// from, skipping over quoted attribute values.
func ScanMailTagEnd(s string, from int) int {
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

// IndexMailCloseTag locates the close tag for a
// drop-with-contents element, case-insensitively.
func IndexMailCloseTag(s string, from int, name string) int {
	idx := strings.Index(strings.ToLower(s[from:]), "</"+name)
	if idx < 0 {
		return -1
	}
	return from + idx
}

// MailHref extracts and validates the href of an <a> tag's
// attribute text: only absolute http(s) URLs with a host survive
// (EVE's showinfo:/javascript:/relative links do not).
func MailHref(attrs string) (string, bool) {
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
