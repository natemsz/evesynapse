package app

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"evesynapse/internal/htmlsanitize"
)

// The changelog page renders the project's changelog in the
// app's own shell. The embedded file is a byte copy of the
// CHANGELOG.md at the repository root; a test pins them equal
// so the page can never drift from the canonical text.
//
// Everything is parsed and rendered once at startup into
// changelogStore: requests only look up the requested version
// node and pass its pre-rendered HTML to the template.

//go:embed changelog.md
var changelogText string

type changelogOption struct {
	Value    string
	Label    string
	Selected bool
}

type changelogMinorLink struct {
	Label  string
	URL    string
	Active bool
}

type changelogView struct {
	Heading       string
	Subheading    string
	IsLatestMinor bool
	IsSingleRev   bool
	Body          template.HTML
	IndexJSON     template.JS

	Releases   []changelogOption
	Majors     []changelogOption
	Minors     []changelogOption
	Revisions  []changelogOption
	MinorLinks []changelogMinorLink
}

type changelogEntry struct {
	Category string
	Summary  string
	Author   string
	Tag      string
}

type changelogRevision struct {
	Release    int
	Major      int
	Minor      int
	Rev        int
	Tag        string
	Date       string
	Subheading string
	Entries    []changelogEntry
	Body       template.HTML
}

type changelogMinor struct {
	Release          int
	Major            int
	Minor            int
	Label            string
	Subheading       string
	Revisions        []*changelogRevision
	byRev            map[int]*changelogRevision
	ConsolidatedHTML template.HTML
}

type changelogMajor struct {
	Release int
	Major   int
	Minors  []*changelogMinor
	byMinor map[int]*changelogMinor
}

type changelogRelease struct {
	Release int
	Majors  []*changelogMajor
	byMajor map[int]*changelogMajor
}

type changelogIndex struct {
	Releases  []*changelogRelease
	byRelease map[int]*changelogRelease
	IndexJSON template.JS
}

// changelogStore is built once at package init from the embedded
// changelog text. No request parses markdown or regroups entries.
var changelogStore = parseChangelog(changelogText)

var (
	reChangelogVersion = regexp.MustCompile(`^## \[v(\d+)\.(\d+)\.(\d+)\.(\d+)\](?:\s*-\s*(\S+))?\s*$`)
	reChangelogAuthor  = regexp.MustCompile(`^(.*?)\s+by\s+(@\S+)(?:\s+in\s+(?:#\d+|https?://\S+))?\s*$`)
)

// canonicalChangelogCategories defines the display order for
// known category headings; any other heading follows in
// first-seen order.
var canonicalChangelogCategories = []string{
	"🚀 Features",
	"🐛 Bug Fixes",
	"🧰 Maintenance & Dependencies",
	"Other Changes",
	"Added",
	"Changed",
	"Fixed",
}

func isChangelogBoilerplate(line string) bool {
	if strings.HasPrefix(line, "Automated build of EveSynapse ") {
		return true
	}
	if strings.HasPrefix(line, "If this is your first install") {
		return true
	}
	if line == "No changes recorded for this release window." {
		return true
	}
	if strings.HasPrefix(line, "No merged pull requests in the interval") {
		return true
	}
	return false
}

func parseChangelogBullet(raw, category, tag string) (changelogEntry, bool) {
	text := strings.TrimSpace(raw)
	if text == "" || isChangelogBoilerplate(text) {
		return changelogEntry{}, false
	}
	if category == "" {
		category = "Other Changes"
	}
	summary := text
	author := ""
	if m := reChangelogAuthor.FindStringSubmatch(text); m != nil {
		if s := strings.TrimSpace(m[1]); s != "" {
			summary = s
			author = m[2]
		}
	}
	return changelogEntry{
		Category: category,
		Summary:  summary,
		Author:   author,
		Tag:      tag,
	}, true
}

func formatChangelogEntry(e changelogEntry) string {
	if e.Author != "" {
		return fmt.Sprintf("%s (%s) (%s)", e.Summary, e.Author, e.Tag)
	}
	return fmt.Sprintf("%s (%s)", e.Summary, e.Tag)
}

func renderChangelogEntries(entries []changelogEntry, emptyNote string) template.HTML {
	if len(entries) == 0 {
		return htmlsanitize.Markdown(emptyNote)
	}

	byCat := make(map[string][]string)
	var seenOrder []string
	for _, e := range entries {
		if _, ok := byCat[e.Category]; !ok {
			seenOrder = append(seenOrder, e.Category)
		}
		byCat[e.Category] = append(byCat[e.Category], formatChangelogEntry(e))
	}

	orderedCats := make([]string, 0, len(byCat))
	used := make(map[string]bool, len(byCat))
	for _, cat := range canonicalChangelogCategories {
		if _, ok := byCat[cat]; ok {
			orderedCats = append(orderedCats, cat)
			used[cat] = true
		}
	}
	for _, cat := range seenOrder {
		if !used[cat] {
			orderedCats = append(orderedCats, cat)
			used[cat] = true
		}
	}

	var sb strings.Builder
	for i, cat := range orderedCats {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("### ")
		sb.WriteString(cat)
		sb.WriteString("\n")
		for _, item := range byCat[cat] {
			sb.WriteString("* ")
			sb.WriteString(item)
			sb.WriteString("\n")
		}
	}
	return htmlsanitize.Markdown(escapeUnderscores(sb.String()))
}

func parseChangelog(src string) *changelogIndex {
	idx := &changelogIndex{
		byRelease: make(map[int]*changelogRelease),
	}

	var curRev *changelogRevision
	curCat := ""
	var revisions []*changelogRevision

	for _, rawLine := range strings.Split(src, "\n") {
		line := strings.TrimSpace(rawLine)
		if m := reChangelogVersion.FindStringSubmatch(line); m != nil {
			rel, _ := strconv.Atoi(m[1])
			maj, _ := strconv.Atoi(m[2])
			min, _ := strconv.Atoi(m[3])
			rev, _ := strconv.Atoi(m[4])
			curRev = &changelogRevision{
				Release: rel,
				Major:   maj,
				Minor:   min,
				Rev:     rev,
				Tag:     fmt.Sprintf("v%d.%d.%d.%d", rel, maj, min, rev),
				Date:    m[5],
			}
			revisions = append(revisions, curRev)
			curCat = ""
			continue
		}
		if curRev == nil {
			continue
		}
		if strings.HasPrefix(line, "## [") {
			// Any other non-4-part version header ends the current revision.
			curRev = nil
			curCat = ""
			continue
		}
		if strings.HasPrefix(line, "### ") {
			curCat = strings.TrimSpace(strings.TrimPrefix(line, "### "))
			continue
		}
		if strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "- ") {
			if entry, ok := parseChangelogBullet(line[2:], curCat, curRev.Tag); ok {
				curRev.Entries = append(curRev.Entries, entry)
			}
		}
	}

	// Build strict release -> major -> minor -> revision tree.
	for _, rev := range revisions {
		relNode, ok := idx.byRelease[rev.Release]
		if !ok {
			relNode = &changelogRelease{
				Release: rev.Release,
				byMajor: make(map[int]*changelogMajor),
			}
			idx.byRelease[rev.Release] = relNode
			idx.Releases = append(idx.Releases, relNode)
		}

		majNode, ok := relNode.byMajor[rev.Major]
		if !ok {
			majNode = &changelogMajor{
				Release: rev.Release,
				Major:   rev.Major,
				byMinor: make(map[int]*changelogMinor),
			}
			relNode.byMajor[rev.Major] = majNode
			relNode.Majors = append(relNode.Majors, majNode)
		}

		minNode, ok := majNode.byMinor[rev.Minor]
		if !ok {
			minNode = &changelogMinor{
				Release: rev.Release,
				Major:   rev.Major,
				Minor:   rev.Minor,
				Label:   fmt.Sprintf("%d.%d.%d.x", rev.Release, rev.Major, rev.Minor),
				byRev:   make(map[int]*changelogRevision),
			}
			majNode.byMinor[rev.Minor] = minNode
			majNode.Minors = append(majNode.Minors, minNode)
		}

		// If the same revision appears twice in raw text, merge its entries.
		if existing, dup := minNode.byRev[rev.Rev]; dup {
			existing.Entries = append(existing.Entries, rev.Entries...)
			if existing.Date == "" && rev.Date != "" {
				existing.Date = rev.Date
			}
			continue
		}
		minNode.byRev[rev.Rev] = rev
		minNode.Revisions = append(minNode.Revisions, rev)
	}

	// Sort every level newest first and pre-render all HTML at startup.
	sort.Slice(idx.Releases, func(i, j int) bool {
		return idx.Releases[i].Release > idx.Releases[j].Release
	})

	type jsonMinor struct {
		V int   `json:"v"`
		R []int `json:"r"`
	}
	type jsonMajor struct {
		V int         `json:"v"`
		N []jsonMinor `json:"n"`
	}
	type jsonRelease struct {
		V int         `json:"v"`
		M []jsonMajor `json:"m"`
	}

	treeJSON := make([]jsonRelease, 0, len(idx.Releases))
	for _, rel := range idx.Releases {
		sort.Slice(rel.Majors, func(i, j int) bool {
			return rel.Majors[i].Major > rel.Majors[j].Major
		})
		jRel := jsonRelease{V: rel.Release, M: make([]jsonMajor, 0, len(rel.Majors))}
		for _, maj := range rel.Majors {
			sort.Slice(maj.Minors, func(i, j int) bool {
				return maj.Minors[i].Minor > maj.Minors[j].Minor
			})
			jMaj := jsonMajor{V: maj.Major, N: make([]jsonMinor, 0, len(maj.Minors))}
			for _, min := range maj.Minors {
				sort.Slice(min.Revisions, func(i, j int) bool {
					return min.Revisions[i].Rev > min.Revisions[j].Rev
				})

				var consolidated []changelogEntry
				revNums := make([]int, 0, len(min.Revisions))
				for _, rev := range min.Revisions {
					revNums = append(revNums, rev.Rev)
					if rev.Date != "" {
						rev.Subheading = fmt.Sprintf("Build %s (%s) in %s", rev.Tag, rev.Date, min.Label)
					} else {
						rev.Subheading = fmt.Sprintf("Build %s in %s", rev.Tag, min.Label)
					}
					rev.Body = renderChangelogEntries(rev.Entries, "No changes were recorded for this build.")
					consolidated = append(consolidated, rev.Entries...)
				}

				min.ConsolidatedHTML = renderChangelogEntries(consolidated, "No changes were recorded for this release series.")
				newest := min.Revisions[0]
				oldest := min.Revisions[len(min.Revisions)-1]
				if len(min.Revisions) == 1 {
					if newest.Date != "" {
						min.Subheading = fmt.Sprintf("1 build (%s, %s)", newest.Tag, newest.Date)
					} else {
						min.Subheading = fmt.Sprintf("1 build (%s)", newest.Tag)
					}
				} else {
					min.Subheading = fmt.Sprintf("%d builds (%s to %s)", len(min.Revisions), oldest.Tag, newest.Tag)
				}

				jMaj.N = append(jMaj.N, jsonMinor{V: min.Minor, R: revNums})
			}
			jRel.M = append(jRel.M, jMaj)
		}
		treeJSON = append(treeJSON, jRel)
	}

	if raw, err := json.Marshal(treeJSON); err == nil {
		idx.IndexJSON = template.JS(raw)
	} else {
		idx.IndexJSON = template.JS("[]")
	}

	return idx
}

func (idx *changelogIndex) view(releaseQ, majorQ, minorQ, revQ string) *changelogView {
	if idx == nil || len(idx.Releases) == 0 {
		return &changelogView{
			Heading:   "Changelog",
			Body:      htmlsanitize.Markdown("No release notes are available."),
			IndexJSON: template.JS("[]"),
		}
	}

	// Resolve release, clamping invalid values to newest.
	rel := idx.Releases[0]
	parentMatched := true
	if releaseQ != "" {
		if n, err := strconv.Atoi(releaseQ); err == nil {
			if found, ok := idx.byRelease[n]; ok {
				rel = found
			} else {
				parentMatched = false
			}
		} else {
			parentMatched = false
		}
	}

	// Resolve major under the chosen release.
	maj := rel.Majors[0]
	if parentMatched && majorQ != "" {
		if n, err := strconv.Atoi(majorQ); err == nil {
			if found, ok := rel.byMajor[n]; ok {
				maj = found
			} else {
				parentMatched = false
			}
		} else {
			parentMatched = false
		}
	}

	// Resolve minor under the chosen release.major.
	min := maj.Minors[0]
	if parentMatched && minorQ != "" {
		if n, err := strconv.Atoi(minorQ); err == nil {
			if found, ok := maj.byMinor[n]; ok {
				min = found
			} else {
				parentMatched = false
			}
		} else {
			parentMatched = false
		}
	}

	// Resolve optional single revision under the chosen minor.
	var selectedRev *changelogRevision
	if parentMatched && revQ != "" && revQ != "all" {
		if n, err := strconv.Atoi(revQ); err == nil {
			if found, ok := min.byRev[n]; ok {
				selectedRev = found
			}
		}
	}

	v := &changelogView{
		IndexJSON:  idx.IndexJSON,
		Releases:   make([]changelogOption, 0, len(idx.Releases)),
		Majors:     make([]changelogOption, 0, len(rel.Majors)),
		Minors:     make([]changelogOption, 0, len(maj.Minors)),
		Revisions:  make([]changelogOption, 0, len(min.Revisions)+1),
		MinorLinks: make([]changelogMinorLink, 0, len(maj.Minors)),
	}

	for _, r := range idx.Releases {
		v.Releases = append(v.Releases, changelogOption{
			Value:    strconv.Itoa(r.Release),
			Label:    strconv.Itoa(r.Release),
			Selected: r.Release == rel.Release,
		})
	}
	for _, m := range rel.Majors {
		v.Majors = append(v.Majors, changelogOption{
			Value:    strconv.Itoa(m.Major),
			Label:    strconv.Itoa(m.Major),
			Selected: m.Major == maj.Major,
		})
	}
	for _, mn := range maj.Minors {
		v.Minors = append(v.Minors, changelogOption{
			Value:    strconv.Itoa(mn.Minor),
			Label:    strconv.Itoa(mn.Minor),
			Selected: mn.Minor == min.Minor,
		})
		v.MinorLinks = append(v.MinorLinks, changelogMinorLink{
			Label:  mn.Label,
			URL:    fmt.Sprintf("/changelog/?release=%d&major=%d&minor=%d", rel.Release, maj.Major, mn.Minor),
			Active: mn.Minor == min.Minor && selectedRev == nil,
		})
	}

	v.Revisions = append(v.Revisions, changelogOption{
		Value:    "",
		Label:    "All revisions",
		Selected: selectedRev == nil,
	})
	for _, rv := range min.Revisions {
		v.Revisions = append(v.Revisions, changelogOption{
			Value:    strconv.Itoa(rv.Rev),
			Label:    rv.Tag,
			Selected: selectedRev != nil && rv.Rev == selectedRev.Rev,
		})
	}

	newestRel := idx.Releases[0]
	newestMaj := newestRel.Majors[0]
	newestMin := newestMaj.Minors[0]
	v.IsLatestMinor = selectedRev == nil && rel == newestRel && maj == newestMaj && min == newestMin

	if selectedRev != nil {
		v.IsSingleRev = true
		v.Heading = selectedRev.Tag
		v.Subheading = selectedRev.Subheading
		v.Body = selectedRev.Body
	} else {
		v.Heading = min.Label
		v.Subheading = min.Subheading
		v.Body = min.ConsolidatedHTML
	}

	return v
}

// escapeUnderscores keeps snake_case names (LOW_RAM_MODE and
// its kin) literal in the render: in the notice renderer a _
// marks italics, and a changelog entry is a title, not prose,
// so its underscores are text. A word carrying an address
// keeps its underscores: an escape inside one would break it.
func escapeUnderscores(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		words := strings.Split(line, " ")
		for j, w := range words {
			if !strings.Contains(w, "://") {
				words[j] = strings.ReplaceAll(w, "_", `\_`)
			}
		}
		lines[i] = strings.Join(words, " ")
	}
	return strings.Join(lines, "\n")
}

// handleChangelog renders the public changelog page. Public for
// the same reason the About page is: what each release changed
// should be readable without an account.
func (app *Application) handleChangelog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data := app.publicPage(r.Context())
	data.Changelog = changelogStore.view(
		strings.TrimSpace(q.Get("release")),
		strings.TrimSpace(q.Get("major")),
		strings.TrimSpace(q.Get("minor")),
		strings.TrimSpace(q.Get("rev")),
	)
	app.render(r.Context(), w, http.StatusOK, "changelog.html", data)
}
