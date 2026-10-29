package htmlsanitize

import (
	"html"
	"html/template"
	"net/url"
	"strings"
)

// Markdown renders the small Markdown subset an administrator's notice
// post is written in, and returns display HTML that has also passed
// through Sanitize.
//
// The subset: # headings, paragraphs, - and 1. lists, > quotes,
// ``` fenced blocks, --- rules, **bold**, *italic* or _italic_,
// ~~struck~~, [text](https://...) links and bare https:// addresses.
// An image, ![alt](url), becomes a link to it: notices show no
// pictures.
//
// Nothing in the source is ever markup. Every character is escaped
// as it is written, so a <script> or an onclick= in the source shows
// as the text it is, and the only tags in the output are the ones
// this renderer writes itself. Links keep only absolute http and
// https addresses. The result then goes through Sanitize, the same
// allow-list mail and bios pass through, as a second line: whatever
// this renderer might get wrong, that one still holds.
func Markdown(src string) template.HTML {
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(src, "\n")
	var out strings.Builder
	var para []string
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		out.WriteString("<p>")
		for i, l := range para {
			if i > 0 {
				out.WriteString("<br>")
			}
			writeInline(&out, l)
		}
		out.WriteString("</p>")
		para = para[:0]
	}

	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		trim := strings.TrimLeft(line, " \t")
		switch {
		case trim == "":
			flushPara()
		case strings.HasPrefix(trim, "```"):
			flushPara()
			out.WriteString("<pre>")
			first := true
			for i++; i < len(lines); i++ {
				if strings.HasPrefix(strings.TrimLeft(lines[i], " \t"), "```") {
					break
				}
				if !first {
					out.WriteString("\n")
				}
				first = false
				out.WriteString(html.EscapeString(lines[i]))
			}
			out.WriteString("</pre>")
		case isRule(trim):
			flushPara()
			out.WriteString("<hr>")
		case headingLevel(trim) > 0:
			flushPara()
			level := headingLevel(trim)
			tag := [...]string{"", "h3", "h4", "h5", "h5", "h5", "h5"}[level]
			out.WriteString("<" + tag + ">")
			writeInline(&out, strings.TrimSpace(trim[level:]))
			out.WriteString("</" + tag + ">")
		case strings.HasPrefix(trim, ">"):
			flushPara()
			out.WriteString("<blockquote>")
			for ; i < len(lines); i++ {
				q := strings.TrimLeft(lines[i], " \t")
				if !strings.HasPrefix(q, ">") {
					i--
					break
				}
				if q = strings.TrimSpace(q[1:]); q != "" {
					out.WriteString("<p>")
					writeInline(&out, q)
					out.WriteString("</p>")
				}
			}
			out.WriteString("</blockquote>")
		case listItem(trim, false) != "", listItem(trim, true) != "":
			flushPara()
			ordered := listItem(trim, true) != ""
			tag := "ul"
			if ordered {
				tag = "ol"
			}
			out.WriteString("<" + tag + ">")
			for ; i < len(lines); i++ {
				item := listItem(strings.TrimLeft(lines[i], " \t"), ordered)
				if item == "" {
					i--
					break
				}
				out.WriteString("<li>")
				writeInline(&out, strings.TrimSpace(item))
				out.WriteString("</li>")
			}
			out.WriteString("</" + tag + ">")
		default:
			para = append(para, trim)
		}
	}
	flushPara()
	return Sanitize(out.String())
}

// headingLevel is the number of leading #s when they make a heading.
func headingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(s) || s[n] != ' ' {
		return 0
	}
	return n
}

// isRule reports a line of three or more dashes, stars or underscores.
func isRule(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 3 {
		return false
	}
	for _, c := range []byte{'-', '*', '_'} {
		if strings.Count(s, string(c)) == len(s) {
			return true
		}
	}
	return false
}

// listItem returns the text of a list line, or "" when the line is
// not one: "- ", "* " or "+ " for a bullet, "1. " for a number.
func listItem(s string, ordered bool) string {
	if !ordered {
		if len(s) >= 2 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && s[1] == ' ' {
			if rest := strings.TrimSpace(s[2:]); rest != "" {
				return rest
			}
		}
		return ""
	}
	n := 0
	for n < len(s) && n < 4 && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n == 0 || n+1 >= len(s) || (s[n] != '.' && s[n] != ')') || s[n+1] != ' ' {
		return ""
	}
	return strings.TrimSpace(s[n+2:])
}

// inlineMaxDepth stops emphasis nesting inside emphasis from
// recursing without end on a line built to make it.
const inlineMaxDepth = 8

func writeInline(out *strings.Builder, s string) {
	writeInlineDepth(out, s, 0)
}

func writeInlineDepth(out *strings.Builder, s string, depth int) {
	var text strings.Builder
	flush := func() {
		out.WriteString(html.EscapeString(text.String()))
		text.Reset()
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && strings.IndexByte("\\`*_~[]()!#>-+.", s[i+1]) >= 0:
			text.WriteByte(s[i+1])
			i += 2
			continue
		case c == '!' && strings.HasPrefix(s[i+1:], "["):
			if label, href, n, ok := parseLink(s[i+1:]); ok {
				flush()
				writeLink(out, label, href, depth, true)
				i += 1 + n
				continue
			}
		case c == '[':
			if label, href, n, ok := parseLink(s[i:]); ok {
				flush()
				writeLink(out, label, href, depth, false)
				i += n
				continue
			}
		case (c == 'h' || c == 'H') && (i == 0 || isSpaceByte(s[i-1]) || s[i-1] == '(') && bareURLAt(s[i:]) > 0:
			n := bareURLAt(s[i:])
			flush()
			href := s[i : i+n]
			out.WriteString(`<a href="` + html.EscapeString(href) + `">` + html.EscapeString(href) + `</a>`)
			i += n
			continue
		case depth < inlineMaxDepth && (c == '*' || c == '_' || c == '~'):
			if tag, inner, n, ok := parseEmphasis(s[i:]); ok {
				flush()
				out.WriteString("<" + tag + ">")
				writeInlineDepth(out, inner, depth+1)
				out.WriteString("</" + tag + ">")
				i += n
				continue
			}
		}
		text.WriteByte(c)
		i++
	}
	flush()
}

// parseEmphasis reads **x**, __x__, *x*, _x_ or ~~x~~ at the start of
// s: the tag, the text inside, and how many bytes it took.
func parseEmphasis(s string) (tag, inner string, n int, ok bool) {
	var delim string
	switch {
	case strings.HasPrefix(s, "**"), strings.HasPrefix(s, "__"):
		delim, tag = s[:2], "strong"
	case strings.HasPrefix(s, "~~"):
		delim, tag = "~~", "s"
	case s[0] == '*' || s[0] == '_':
		delim, tag = s[:1], "em"
	default:
		return "", "", 0, false
	}
	rest := s[len(delim):]
	if rest == "" || isSpaceByte(rest[0]) {
		return "", "", 0, false
	}
	end := strings.Index(rest, delim)
	if end <= 0 || isSpaceByte(rest[end-1]) {
		return "", "", 0, false
	}
	return tag, rest[:end], len(delim) + end + len(delim), true
}

// parseLink reads [label](href) at the start of s.
func parseLink(s string) (label, href string, n int, ok bool) {
	mid := strings.Index(s, "](")
	if !strings.HasPrefix(s, "[") || mid < 1 {
		return "", "", 0, false
	}
	end := strings.IndexByte(s[mid+2:], ')')
	if end < 0 {
		return "", "", 0, false
	}
	return s[1:mid], strings.TrimSpace(s[mid+2 : mid+2+end]), mid + 2 + end + 1, true
}

// writeLink writes a link when its address is absolute http or https,
// and the label alone when it is anything else.
func writeLink(out *strings.Builder, label, href string, depth int, image bool) {
	if image && label == "" {
		label = href
	}
	if !SafeLinkURL(href) {
		writeInlineDepth(out, label, inlineMaxDepth)
		return
	}
	out.WriteString(`<a href="` + html.EscapeString(href) + `">`)
	if image {
		out.WriteString(html.EscapeString(label))
	} else {
		writeInlineDepth(out, label, depth+1)
	}
	out.WriteString("</a>")
}

// SafeLinkURL reports whether s is an absolute http or https address
// with a host: the only links a notice keeps.
func SafeLinkURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == "http" || scheme == "https"
}

// bareURLAt returns the length of a bare http(s) address at the start
// of s, trailing sentence punctuation left out, or 0.
func bareURLAt(s string) int {
	lower := strings.ToLower(s[:min(len(s), 8)])
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		return 0
	}
	n := 0
	for n < len(s) && !isSpaceByte(s[n]) && s[n] != '<' && s[n] != '>' && s[n] != '"' {
		n++
	}
	for n > 0 && strings.IndexByte(".,;:!?)'", s[n-1]) >= 0 {
		n--
	}
	if !SafeLinkURL(s[:n]) {
		return 0
	}
	return n
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n'
}
