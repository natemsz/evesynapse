package app

// Tests that the HTML sanitizer's output is always well-formed on
// its own. It renders text written by other players (mail, pilot
// bios, corporation descriptions) inside the app's pages, so an
// unbalanced result would let that text close the page's own
// containers and draw outside the box it is shown in.

import (
	"regexp"
	"strings"
	"testing"
)

var sanitizedTagPattern = regexp.MustCompile(`<(/?)([a-z0-9]+)[^>]*>`)

// assertWellFormed fails unless every tag in out is closed in the
// right order and nothing is closed that was not opened.
func assertWellFormed(t *testing.T, in, out string) {
	t.Helper()
	var open []string
	for _, m := range sanitizedTagPattern.FindAllStringSubmatch(out, -1) {
		closing, name := m[1] == "/", m[2]
		if mailVoidTags[name] {
			continue
		}
		if !closing {
			open = append(open, name)
			continue
		}
		if len(open) == 0 {
			t.Errorf("sanitizeMailHTML(%q) = %q: </%s> closes nothing", in, out, name)
			return
		}
		if top := open[len(open)-1]; top != name {
			t.Errorf("sanitizeMailHTML(%q) = %q: </%s> closes <%s>", in, out, name, top)
			return
		}
		open = open[:len(open)-1]
	}
	if len(open) > 0 {
		t.Errorf("sanitizeMailHTML(%q) = %q: left open: %v", in, out, open)
	}
}

func TestSanitizeMailHTMLOutputIsBalanced(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "stray close tags are dropped",
			in:   `</div></div>text</p>`,
			want: `text`,
		},
		{
			name: "breaking out of the page's containers",
			in:   `</div></div></main><div><h1>Session expired</h1><a href="https://evil.example/login">Sign in again</a>`,
			want: `<div><h1>Session expired</h1><a href="https://evil.example/login" rel="nofollow noopener noreferrer" target="_blank">Sign in again</a></div>`,
		},
		{
			name: "tags left open are closed at the end",
			in:   `<div><b>bold`,
			want: `<div><b>bold</b></div>`,
		},
		{
			name: "crossed tags are untangled",
			in:   `<b><i>x</b>y</i>`,
			want: `<b><i>x</i></b>y`,
		},
		{
			name: "closing an outer element closes what is inside it",
			in:   `<table><tr><td>cell</table>after`,
			want: `<table><tr><td>cell</td></tr></table>after`,
		},
		{
			name: "a dropped link swallows its own close tag only",
			in:   `<a href="javascript:x"><b>t</a></b>`,
			want: `<b>t</b>`,
		},
		{
			name: "a self-closing spelling of a container is still closed",
			in:   `<div/>text`,
			want: `<div>text</div>`,
		},
		{
			name: "line breaks need no closing",
			in:   `a<br>b<hr/>c</br>`,
			want: `a<br>b<hr>c`,
		},
		{
			name: "an unterminated comment still closes what was open",
			in:   `<div><b>x<!-- never ends`,
			want: `<div><b>x</b></div>`,
		},
		{
			name: "an unterminated dropped element still closes what was open",
			in:   `<p>kept<script>alert(1)`,
			want: `<p>kept</p>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(sanitizeMailHTML(tc.in))
			if got != tc.want {
				t.Errorf("sanitizeMailHTML(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			assertWellFormed(t, tc.in, got)
		})
	}
}

func TestSanitizeMailHTMLNestingIsCapped(t *testing.T) {
	in := strings.Repeat("<div>", 5000) + "deep" + strings.Repeat("</div>", 5000)
	got := string(sanitizeMailHTML(in))
	if n := strings.Count(got, "<div>"); n != mailMaxDepth {
		t.Fatalf("%d nested <div> written, want the cap of %d", n, mailMaxDepth)
	}
	if !strings.Contains(got, "deep") {
		t.Fatal("the text inside the over-deep nesting was lost")
	}
	assertWellFormed(t, "5000 nested divs", got)
}

// TestSanitizeMailHTMLAlwaysBalanced throws tag soup at the
// sanitizer: whatever comes in, what goes out is well-formed and
// carries no tag or attribute outside the allowlist.
func TestSanitizeMailHTMLAlwaysBalanced(t *testing.T) {
	pieces := []string{
		"<div>", "</div>", "<b>", "</b>", "<i>", "</i>", "<p>", "</p>",
		"<table>", "</table>", "<tr>", "</tr>", "<td>", "</td>", "<ul>", "<li>", "</li>", "</ul>",
		`<a href="https://example.com/">`, `<a href="javascript:alert(1)">`, `<a>`, "</a>",
		"<script>", "</script>", "<style>", "<img src=x onerror=alert(1)>", "<svg onload=alert(1)>", "</svg>",
		"<br>", "<hr/>", "<!--", "-->", "<", ">", "text", " & ", `"`, "<div onclick=\"x()\" style='y'>", "<DIV>", "</B>",
		"<unknown>", "</unknown>", "<font color=red>", "</font>", "<blockquote>", "</pre>",
	}
	// A small deterministic generator: every run tests the same
	// inputs, so a failure can be reproduced from its message.
	seed := uint32(12345)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>8) % n
	}
	for run := 0; run < 4000; run++ {
		var b strings.Builder
		for k := 0; k < 1+next(14); k++ {
			b.WriteString(pieces[next(len(pieces))])
		}
		in := b.String()
		out := string(sanitizeMailHTML(in))
		assertWellFormed(t, in, out)
		// Every tag in the output is one of the exact forms the
		// sanitizer writes: a bare allowed tag, or a link with a
		// checked http(s) address and the fixed rel/target.
		for _, m := range sanitizedTagPattern.FindAllStringSubmatch(out, -1) {
			tag, name := m[0], m[2]
			switch {
			case !mailAllowedTags[name]:
				t.Errorf("sanitizeMailHTML(%q) = %q: wrote <%s>, which is not allowed", in, out, name)
			case tag == "<"+name+">" || tag == "</"+name+">":
			case name == "a" && linkTagPattern.MatchString(tag):
			default:
				t.Errorf("sanitizeMailHTML(%q) = %q: wrote %q, which carries something it should not", in, out, tag)
			}
		}
		// And nothing outside those tags is markup: all text is
		// escaped.
		if rest := sanitizedTagPattern.ReplaceAllString(out, ""); strings.ContainsAny(rest, "<>") {
			t.Errorf("sanitizeMailHTML(%q) = %q: unescaped markup in the text", in, out)
		}
		if t.Failed() {
			return
		}
	}
}

var linkTagPattern = regexp.MustCompile(`^<a href="https?://[^"<>]*" rel="nofollow noopener noreferrer" target="_blank">$`)
