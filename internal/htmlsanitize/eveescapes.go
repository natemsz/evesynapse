package htmlsanitize

import (
	"strconv"
	"strings"
	"unicode/utf16"
)

// DecodeEVEEscapes turns the escaped text some EVE profile
// descriptions carry back into characters. Older descriptions come
// back from ESI as a Python unicode literal, u'…' with \uXXXX
// escapes for every non-ASCII character, so a Chinese or Russian
// description would otherwise show as rows of escape codes. The
// u'…' wrapper is dropped and each \uXXXX (a surrogate pair joined
// into one character) becomes its character. Text without escapes
// comes back unchanged. Run it before Sanitize: the result is still
// untrusted markup.
func DecodeEVEEscapes(s string) string {
	if len(s) >= 3 && (strings.HasPrefix(s, "u'") && strings.HasSuffix(s, "'") || strings.HasPrefix(s, `u"`) && strings.HasSuffix(s, `"`)) {
		s = s[2 : len(s)-1]
	}
	if !strings.Contains(s, `\u`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := escapeAt(s, i)
		if n == 0 {
			b.WriteByte(s[i])
			i++
			continue
		}
		if utf16.IsSurrogate(r) {
			if low, m := escapeAt(s, i+n); m > 0 {
				if pair := utf16.DecodeRune(r, low); pair != '�' {
					b.WriteRune(pair)
					i += n + m
					continue
				}
			}
		}
		b.WriteRune(r)
		i += n
	}
	return b.String()
}

// escapeAt reads a \uXXXX escape at s[i], returning its code unit
// and length, or 0 length when there is none.
func escapeAt(s string, i int) (rune, int) {
	if i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' {
		return 0, 0
	}
	v, err := strconv.ParseUint(s[i+2:i+6], 16, 32)
	if err != nil {
		return 0, 0
	}
	return rune(v), 6
}

// EVELinkPath maps one of EVE's in-game link schemes to the
// EveSynapse page that shows the same thing: showinfo: to the
// system, pilot, corporation, alliance, station or item page, and
// killReport: to the kill. It serves SanitizeLinked for mail, pilot
// bios and corporation descriptions. Other schemes are not mapped.
func EVELinkPath(href string) (string, bool) {
	switch {
	case strings.HasPrefix(href, "showinfo:"):
		rest := strings.TrimPrefix(href, "showinfo:")
		typePart, itemPart, hasItem := strings.Cut(rest, "//")
		typeID, err := strconv.ParseInt(typePart, 10, 64)
		if err != nil || typeID <= 0 {
			return "", false
		}
		if !hasItem {
			return "/items/type/" + strconv.FormatInt(typeID, 10) + "/", true
		}
		itemID, err := strconv.ParseInt(strings.TrimSpace(itemPart), 10, 64)
		if err != nil || itemID <= 0 {
			return "", false
		}
		id := strconv.FormatInt(itemID, 10)
		switch {
		case typeID == 5:
			return "/system/?system=" + id, true
		case isCharacterType(typeID):
			return "/pilot/?character=" + id, true
		case typeID == 2:
			return "/corporation/?corporation=" + id, true
		case typeID == 16159:
			return "/alliance/?alliance=" + id, true
		case itemID >= 60000000 && itemID < 64000000:
			return "/station/?station=" + id, true
		}
		return "/items/type/" + strconv.FormatInt(typeID, 10) + "/", true
	case strings.HasPrefix(href, "killReport:"):
		idPart, _, _ := strings.Cut(strings.TrimPrefix(href, "killReport:"), ":")
		if id, err := strconv.ParseInt(idPart, 10, 64); err == nil && id > 0 {
			return "/intel/kill/" + strconv.FormatInt(id, 10) + "/", true
		}
	}
	return "", false
}

// isCharacterType reports the character type IDs: the race-specific
// character types and the later shared one.
func isCharacterType(typeID int64) bool {
	return (typeID >= 1373 && typeID <= 1386) || typeID == 34574
}

// FontColor reads a <font color> value as EVE writes it and returns
// it as #rrggbb. EVE puts alpha first (#AARRGGBB); #RRGGBB and #RGB
// are read too. A colour that is fully transparent or too dark to
// read on the site's dark surfaces is refused, so the text keeps
// the page's own colour; so is anything that is not a colour code.
func FontColor(v string) (string, bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "#"))
	var r, g, b, a uint64 = 0, 0, 0, 255
	hex := func(s string) (uint64, bool) {
		n, err := strconv.ParseUint(s, 16, 8)
		return n, err == nil
	}
	ok := true
	switch len(v) {
	case 8:
		var okA, okR, okG, okB bool
		a, okA = hex(v[0:2])
		r, okR = hex(v[2:4])
		g, okG = hex(v[4:6])
		b, okB = hex(v[6:8])
		ok = okA && okR && okG && okB
	case 6:
		var okR, okG, okB bool
		r, okR = hex(v[0:2])
		g, okG = hex(v[2:4])
		b, okB = hex(v[4:6])
		ok = okR && okG && okB
	case 3:
		var okR, okG, okB bool
		r, okR = hex(strings.Repeat(v[0:1], 2))
		g, okG = hex(strings.Repeat(v[1:2], 2))
		b, okB = hex(strings.Repeat(v[2:3], 2))
		ok = okR && okG && okB
	default:
		return "", false
	}
	if !ok || a == 0 {
		return "", false
	}
	// Perceived brightness (ITU-R BT.601 weights), 0 to 255.
	if (299*r+587*g+114*b)/1000 < 70 {
		return "", false
	}
	return "#" + strconv.FormatUint(r>>4, 16) + strconv.FormatUint(r&15, 16) +
		strconv.FormatUint(g>>4, 16) + strconv.FormatUint(g&15, 16) +
		strconv.FormatUint(b>>4, 16) + strconv.FormatUint(b&15, 16), true
}
