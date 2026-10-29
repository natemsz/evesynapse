package htmlsanitize

import (
	"strings"
	"testing"
)

// TestMarkdownNeverPassesMarkup pins the notice renderer's promise:
// raw HTML in the source is text, event handlers never become
// attributes, and only http and https links survive.
func TestMarkdownNeverPassesMarkup(t *testing.T) {
	cases := []struct {
		name, src string
		want      []string // must appear
		reject    []string // must not appear
	}{
		{
			name:   "script tag",
			src:    "hi <script>alert(1)</script>",
			want:   []string{"&lt;script&gt;"},
			reject: []string{"<script"},
		},
		{
			name:   "event handler",
			src:    `<img src=x onerror="alert(1)"> **bold** <a href="https://x.test" onclick="steal()">x</a>`,
			want:   []string{"<strong>bold</strong>", "&lt;img"},
			reject: []string{"<img", "<a href=\"https://x.test\" onclick", `onerror="`, `onclick="`},
		},
		{
			name:   "javascript link",
			src:    "[click](javascript:alert(1)) and [data](data:text/html,x) and [rel](/admin/)",
			want:   []string{"click", "data", "rel"},
			reject: []string{"href=\"javascript", "href=\"data", "href=\"/admin"},
		},
		{
			name: "allowed links",
			src:  "[site](https://evesynapse.test/x?a=1&b=2) and http://plain.test/y.",
			want: []string{
				`<a href="https://evesynapse.test/x?a=1&amp;b=2" rel="nofollow noopener noreferrer" target="_blank">site</a>`,
				`<a href="http://plain.test/y" rel="nofollow noopener noreferrer" target="_blank">http://plain.test/y</a>.`,
			},
		},
		{
			name:   "image becomes a link",
			src:    "![logo](https://elsewhere.test/a.png)",
			want:   []string{`<a href="https://elsewhere.test/a.png"`},
			reject: []string{"<img"},
		},
		{
			name:   "quote breaking out of an href",
			src:    `[x](https://a.test/"onmouseover="alert(1))`,
			reject: []string{`"onmouseover="`, "onmouseover=\"alert"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(Markdown(tc.src))
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("Markdown(%q) = %q, missing %q", tc.src, got, w)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(got, r) {
					t.Errorf("Markdown(%q) = %q, contains %q", tc.src, got, r)
				}
			}
		})
	}
}
