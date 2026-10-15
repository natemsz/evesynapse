// Package tags is the account's character-tag vocabulary: how a
// character's stored tag string splits, and which tags an account uses.
package tags

import (
	"sort"
	"strings"

	db "evesynapse/internal/db/sqlc"
)

// Split reads a stored tag string (comma, space or semicolon separated).
func Split(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';'
	})
}

// InUse lists every tag in use across the account's characters, sorted.
func InUse(chars []db.Character) []string {
	seen := map[string]bool{}
	for _, ch := range chars {
		for _, tag := range Split(ch.Tags) {
			seen[tag] = true
		}
	}
	out := make([]string, 0, len(seen))
	for tag := range seen {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}
