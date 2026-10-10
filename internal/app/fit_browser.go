package app

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The fit browser: what the corporate fits page (corp_doctrines.go)
// and the public fits page (char_fit_public.go) share. A search box,
// filters offered as chips, and the fits as cards.
//
// The filters are worked out from the fits themselves: a chip is
// offered only for a value some fit has, and its count is how many
// fits picking it would leave, given the search and every other
// filter already chosen. Each chip is a plain link, so the page needs
// no script to filter.
// ---------------------------------------------------------------------------

// fitCard is one fit in a browser.
type fitCard struct {
	ID          int64
	Name        string
	Ship        string
	ShipTypeID  int64
	Hull        string // the ship's class: Frigate, Logistics, …
	Role        string // its part in the fleet (a doctrine fit)
	DoctrineID  int64
	Doctrine    string
	DoctrineURL string
	Category    string
	By          string // a public fit's author
	Mine        bool
	Tags        []facetChip // each a link that filters by the tag
	OpenURL     string      // opens it in the fitting tool
	AddField    string      // the add-to-doctrine form's field for this fit
	Updated     time.Time

	tags []string
}

// facetChip is one link in a browser: a filter value with its count,
// a sort order, or a chosen filter to take off again.
type facetChip struct {
	Label string
	Count int
	URL   string
	On    bool
}

type facetGroup struct {
	Title string
	Chips []facetChip
}

// fitFilter is one thing a browser's fits can be narrowed by.
type fitFilter struct {
	Key   string
	Title string
	Value string                  // what the address chose; "" for any
	Of    func(*fitCard) []string // the values a fit has
	Label func(string) string     // how a value is shown; nil for as it is
	Most  int                     // how many chips to offer
}

type browseSort struct {
	Key, Label string
	Less       func(a, b *fitCard) bool
}

// fitBrowser describes one browser page: where it lives, what its
// links must keep, and what it filters and sorts by.
type fitBrowser struct {
	Path    string
	Fixed   url.Values // kept by every link: the corporation, the doctrine being added to
	Q       string
	Sort    string
	Sorts   []browseSort // the first is the default
	Filters []fitFilter
}

type formField struct{ Name, Value string }

// fitBrowserView is what the shared template draws.
type fitBrowserView struct {
	Path        string
	Q           string
	Hidden      []formField // what a search has to carry along
	SuggestURL  string
	Placeholder string
	Facets      []facetGroup
	Active      []facetChip // chosen filters, each a link that takes it off
	ClearURL    string
	Sorts       []facetChip
	Cards       []fitCard
	Total       int  // fits before the search and filters
	More        bool // more match than are shown
	// For is the doctrine a keeper is choosing fits for, when the page
	// was opened from one.
	For   *opChoice
	Roles []string
	Self  string // this page's address as filtered, to come back to
	Empty string // what to say when there are no fits at all
}

// address is the browser's address with some choices changed; an empty
// value takes a choice off.
func (b *fitBrowser) address(changes map[string]string) string {
	query := url.Values{}
	for key, values := range b.Fixed {
		query[key] = values
	}
	set := func(key, value string) {
		if changed, ok := changes[key]; ok {
			value = changed
		}
		if value != "" {
			query.Set(key, value)
		}
	}
	set("q", b.Q)
	if len(b.Sorts) > 0 && b.Sort != b.Sorts[0].Key {
		set("sort", b.Sort)
	} else {
		set("sort", "")
	}
	for _, f := range b.Filters {
		set(f.Key, f.Value)
	}
	if len(query) == 0 {
		return b.Path
	}
	return b.Path + "?" + query.Encode()
}

func hasFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

// run searches, filters and sorts the fits, and builds the page's
// chips from what is left.
func (b *fitBrowser) run(cards []fitCard) *fitBrowserView {
	view := &fitBrowserView{Path: b.Path, Q: b.Q, Total: len(cards), ClearURL: b.Path, Roles: opFleetRoles}
	if len(b.Fixed) > 0 {
		view.ClearURL = b.Path + "?" + b.Fixed.Encode()
	}
	for key, values := range b.Fixed {
		view.Hidden = append(view.Hidden, formField{key, values[0]})
	}
	sort.Slice(view.Hidden, func(i, j int) bool { return view.Hidden[i].Name < view.Hidden[j].Name })

	valid := false
	for _, s := range b.Sorts {
		valid = valid || s.Key == b.Sort
	}
	if !valid && len(b.Sorts) > 0 {
		b.Sort = b.Sorts[0].Key
	}

	// The search first: every count is of fits the search already found.
	needle := strings.ToLower(b.Q)
	var found []*fitCard
	for i := range cards {
		c := &cards[i]
		hay := strings.ToLower(strings.Join([]string{c.Name, c.Ship, c.Hull, c.Doctrine, c.Category, c.By, strings.Join(c.tags, "\n")}, "\n"))
		if needle == "" || strings.Contains(hay, needle) {
			found = append(found, c)
		}
	}
	passes := func(c *fitCard, except string) bool {
		for _, f := range b.Filters {
			if f.Value != "" && f.Key != except && !hasFold(f.Of(c), f.Value) {
				return false
			}
		}
		return true
	}

	for _, f := range b.Filters {
		label := f.Label
		if label == nil {
			label = func(v string) string { return v }
		}
		if f.Value != "" {
			view.Active = append(view.Active, facetChip{Label: f.Title + ": " + label(f.Value), URL: b.address(map[string]string{f.Key: ""}), On: true})
			view.Hidden = append(view.Hidden, formField{f.Key, f.Value})
		}
		counts, shown := map[string]int{}, map[string]string{}
		for _, c := range found {
			if !passes(c, f.Key) {
				continue
			}
			for _, v := range f.Of(c) {
				key := strings.ToLower(v)
				if key == "" {
					continue
				}
				counts[key]++
				if shown[key] == "" {
					shown[key] = v
				}
			}
		}
		group := facetGroup{Title: f.Title}
		for key, n := range counts {
			on := strings.EqualFold(key, f.Value)
			chip := facetChip{Label: label(shown[key]), Count: n, On: on, URL: b.address(map[string]string{f.Key: shown[key]})}
			if on {
				chip.URL = b.address(map[string]string{f.Key: ""})
			}
			group.Chips = append(group.Chips, chip)
		}
		sort.Slice(group.Chips, func(i, j int) bool {
			a, c := group.Chips[i], group.Chips[j]
			if a.On != c.On {
				return a.On
			}
			if a.Count != c.Count {
				return a.Count > c.Count
			}
			return strings.ToLower(a.Label) < strings.ToLower(c.Label)
		})
		if f.Most > 0 && len(group.Chips) > f.Most {
			group.Chips = group.Chips[:f.Most]
		}
		// One value that every fit has narrows nothing; it is not offered.
		if len(group.Chips) > 1 || (len(group.Chips) == 1 && group.Chips[0].On) {
			view.Facets = append(view.Facets, group)
		}
	}

	var kept []*fitCard
	for _, c := range found {
		if passes(c, "") {
			kept = append(kept, c)
		}
	}
	for _, s := range b.Sorts {
		view.Sorts = append(view.Sorts, facetChip{Label: s.Label, On: s.Key == b.Sort, URL: b.address(map[string]string{"sort": s.Key})})
		if s.Key == b.Sort {
			less := s.Less
			sort.SliceStable(kept, func(i, j int) bool { return less(kept[i], kept[j]) })
		}
	}
	if len(b.Sorts) > 0 && b.Sort != b.Sorts[0].Key {
		view.Hidden = append(view.Hidden, formField{"sort", b.Sort})
	}
	if len(b.Sorts) > 0 {
		view.Sorts[0].URL = b.address(map[string]string{"sort": ""})
	}
	for _, c := range kept {
		for _, tag := range c.tags {
			c.Tags = append(c.Tags, facetChip{Label: tag, URL: b.address(map[string]string{"tag": tag})})
		}
		view.Cards = append(view.Cards, *c)
	}
	view.Self = b.address(nil)
	if !strings.Contains(view.Self, "?") {
		view.Self += "?"
	}
	return view
}

// Sort orders the browsers share.
func byFold(of func(*fitCard) string) func(a, b *fitCard) bool {
	return func(a, b *fitCard) bool {
		x, y := strings.ToLower(of(a)), strings.ToLower(of(b))
		if x != y {
			return x < y
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	}
}

var (
	sortFitName = browseSort{"name", "Name", byFold(func(c *fitCard) string { return c.Name })}
	sortFitShip = browseSort{"ship", "Ship", byFold(func(c *fitCard) string { return c.Ship })}
)

// Filters the browsers share.
func filterHull(value string) fitFilter {
	return fitFilter{Key: "hull", Title: "Ship class", Value: value, Most: 14, Of: func(c *fitCard) []string { return []string{c.Hull} }}
}

func filterTag(value string) fitFilter {
	return fitFilter{Key: "tag", Title: "Tag", Value: value, Most: 16, Of: func(c *fitCard) []string { return c.tags }}
}

// hullClasses names the class of each ship: its group in the game's
// data (Frigate, Logistics, Strategic Cruiser, …).
func (app *Application) hullClasses(ctx context.Context, cards []fitCard) {
	seen := map[int64]bool{}
	var ids []int64
	for _, c := range cards {
		if c.ShipTypeID != 0 && !seen[c.ShipTypeID] {
			seen[c.ShipTypeID] = true
			ids = append(ids, c.ShipTypeID)
		}
	}
	if len(ids) == 0 {
		return
	}
	rows, err := app.queries.ListTypeGroupNames(ctx, ids)
	if err != nil {
		logging.Errorf("fit browser: ship classes: %v", err)
		return
	}
	class := map[int64]string{}
	for _, row := range rows {
		class[row.TypeID] = row.Name
	}
	for i := range cards {
		cards[i].Hull = class[cards[i].ShipTypeID]
	}
}

// suggestFits offers what a browser's search box suggests while
// typing: fits (a pick opens one in the fitting tool), then the
// filters a word could mean, each a link to the browser so filtered.
func (b *fitBrowser) suggestFits(cards []fitCard, q string) []suggestItem {
	needle := strings.ToLower(strings.TrimSpace(q))
	if needle == "" {
		return nil
	}
	var out []suggestItem
	seen := map[string]bool{}
	add := func(name, label, address string) {
		key := label + "\n" + strings.ToLower(name)
		if name == "" || seen[key] || len(out) >= suggestBrowseMost || !strings.Contains(strings.ToLower(name), needle) {
			return
		}
		seen[key] = true
		out = append(out, suggestItem{Name: name, Label: label, URL: address})
	}
	for i := range cards {
		c := &cards[i]
		label := c.Ship
		if c.Doctrine != "" {
			label += " · " + c.Doctrine
		} else if c.By != "" {
			label += " · by " + c.By
		}
		if len(out) < suggestBrowseFits {
			add(c.Name, label, c.OpenURL)
		}
	}
	for i := range cards {
		c := &cards[i]
		add(c.Ship, "Ship", b.address(map[string]string{"q": c.Ship}))
		for _, f := range b.Filters {
			for _, v := range f.Of(c) {
				shown := v
				if f.Label != nil {
					shown = f.Label(v)
				}
				if strings.Contains(strings.ToLower(shown), needle) {
					key := f.Title + "\n" + strings.ToLower(shown)
					if !seen[key] && len(out) < suggestBrowseMost {
						seen[key] = true
						out = append(out, suggestItem{Name: shown, Label: f.Title, URL: b.address(map[string]string{f.Key: v, "q": ""})})
					}
				}
			}
		}
	}
	return out
}

const (
	suggestBrowseMost = 12
	suggestBrowseFits = 6
)

func idString(id int64) string { return strconv.FormatInt(id, 10) }
