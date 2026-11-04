package app

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/tags"
)

// charScope is a page's character scope: every character on the
// account, or a chosen set of them. Pages that read several
// characters at once (Assets, My Jobs) share it, with the
// charselectormulti control (assetScopeSelector) over it.
type charScope struct {
	all bool
	ids []int64 // the selected characters, in account order, when not all
}

// parseCharScope reads a scope parameter: absent or "all" is every
// character, "none" the explicit empty set, otherwise a
// comma-separated set of character IDs. Unknown IDs drop out, and a
// set covering the whole account (or nothing valid) reads as all.
func parseCharScope(raw string, characters []db.Character) charScope {
	switch raw {
	case "", "all":
		return charScope{all: true}
	case "none":
		return charScope{ids: []int64{}}
	}
	picked := make(map[int64]bool)
	for _, part := range strings.Split(raw, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
			picked[id] = true
		}
	}
	var ids []int64
	for _, ch := range characters {
		if picked[ch.CharacterID] {
			ids = append(ids, ch.CharacterID)
		}
	}
	if len(ids) == 0 || len(ids) == len(characters) {
		return charScope{all: true}
	}
	return charScope{ids: ids}
}

func (s charScope) has(id int64) bool {
	if s.all {
		return true
	}
	for _, x := range s.ids {
		if x == id {
			return true
		}
	}
	return false
}

// param is the scope's query value; "" for all characters.
func (s charScope) param() string {
	if s.all {
		return ""
	}
	if len(s.ids) == 0 {
		return "none"
	}
	return joinIDList(s.ids)
}

// buildScopeSelector shapes the scope control: a reset row plus one
// checklist row per character, each row's link toggling that
// character in or out of the scope. Toggling from all characters
// starts from the whole account; a complete set reads back as all,
// and an empty one is the explicit none scope. href builds a link to
// the page with the given scope value ("" for all).
func buildScopeSelector(characters []db.Character, s charScope, href func(scope string) string) assetScopeSelector {
	sel := assetScopeSelector{All: s.all}
	allScope := ""
	if s.all {
		allScope = "none"
	}
	sel.AllHref = href(allScope)
	var inScope []db.Character
	for _, ch := range characters {
		if s.has(ch.CharacterID) {
			inScope = append(inScope, ch)
		}
		set := make(map[int64]bool)
		for _, other := range characters {
			if s.has(other.CharacterID) {
				set[other.CharacterID] = true
			}
		}
		if set[ch.CharacterID] {
			delete(set, ch.CharacterID)
		} else {
			set[ch.CharacterID] = true
		}
		var next []int64
		for _, other := range characters {
			if set[other.CharacterID] {
				next = append(next, other.CharacterID)
			}
		}
		param := ""
		switch {
		case len(next) == 0:
			param = "none"
		case len(next) < len(characters):
			param = joinIDList(next)
		}
		sel.Chars = append(sel.Chars, assetCharOption{
			ID: ch.CharacterID, Name: ch.Name, Image: portraitURL(ch.CharacterID, 64),
			Tags: tags.Split(ch.Tags), TagsText: ch.Tags, Checked: s.has(ch.CharacterID),
			ToggleHref: href(param),
			OnlyHref:   href(strconv.FormatInt(ch.CharacterID, 10)),
		})
	}
	switch {
	case s.all:
		sel.Label = "All characters"
	case len(inScope) == 0:
		sel.Label = "No characters"
	case len(inScope) <= 2:
		names := make([]string, len(inScope))
		for i, ch := range inScope {
			names[i] = ch.Name
		}
		sel.Label = strings.Join(names, ", ")
	default:
		sel.Label = fmt.Sprintf("%d characters", len(inScope))
	}
	return sel
}

// withQuery returns base with v's values set (empty values removed).
func withQuery(base string, v url.Values) string {
	for k, vals := range v {
		if len(vals) == 0 || vals[0] == "" {
			v.Del(k)
		}
	}
	if enc := v.Encode(); enc != "" {
		return base + "?" + enc
	}
	return base
}
