// Package scan reads the text players copy out of the EVE client --
// the directional scanner, the local chat member list, the fleet
// composition window, the probe scanner -- into rows. It is pure:
// no names are looked up and nothing is fetched here; the app
// (internal/app/scans.go) enriches the rows and stores the result.
//
// The four pastes, as the client copies them:
//
//	d-scan  typeID <tab> item name <tab> type name <tab> distance
//	local   one character name a line
//	fleet   name <tab> system <tab> ship <tab> ship class <tab> position <tab> skills <tab> wing / squad
//	probe   ABC-123 <tab> group <tab> site type <tab> name <tab> signal % <tab> distance
//
// Which paste it is is told from the lines themselves (Detect), so
// one box takes them all.
package scan

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Kind is which paste a scan is.
type Kind string

const (
	KindDScan Kind = "dscan"
	KindLocal Kind = "local"
	KindFleet Kind = "fleet"
	KindProbe Kind = "probe"
)

// Limits on what one paste may hold.
const (
	MaxBytes = 256 << 10
	MaxLines = 5000
	// MaxLocal caps a local paste's names (Jita local runs ~2000;
	// beyond this the resolution would cost more than it tells).
	MaxLocal = 1500
)

// Bounds on the residue kept with a reading: the lines of a
// paste the parser did not consume. The capture is bookkeeping
// for later classification work, so it stays small: at most
// MaxResidueSpans lines and MaxResidueBytes of text in all.
const (
	MaxResidueSpans = 40
	MaxResidueBytes = 2048
)

// ErrEmpty and ErrUnknown are Parse's two refusals.
var (
	ErrEmpty   = errors.New("nothing to read")
	ErrUnknown = errors.New("not a scan the tool knows")
	ErrTooBig  = errors.New("too large")
)

// AUKm is one astronomical unit in kilometres.
const AUKm = 149597870.7

// DScanRow is one line of a directional scan.
type DScanRow struct {
	TypeID   int64
	Name     string // the item's own name ("Jita IV - Moon 4", a pilot's ship name)
	TypeName string
	// DistKm is the distance in kilometres, -1 when the client
	// showed none ("-": beyond range of a figure, or a celestial).
	DistKm float64
}

// FleetRow is one member of a fleet composition paste.
type FleetRow struct {
	Name, System, Ship, Class, Position, Wing string
}

// ProbeRow is one probe scanner result.
type ProbeRow struct {
	ID, Group, Type, Name string
	Signal                float64 // percent
	DistKm                float64 // -1 when not shown
}

// Result is a parsed paste: Kind, and the rows of that kind.
type Result struct {
	Kind  Kind
	DScan []DScanRow
	Local []string
	Fleet []FleetRow
	Probe []ProbeRow
	// Skipped counts lines that did not read as the paste's kind.
	Skipped int
	// Residue is those skipped lines themselves, kept (bounded
	// by MaxResidueSpans and MaxResidueBytes) with the reading
	// they arrived in: the part of the input the parser did not
	// consume, recorded as it stood. A line dropped only for
	// running past MaxLocal was consumed and is not residue.
	Residue []string
}

// addResidue keeps one unconsumed line, within the bounds.
func (r *Result) addResidue(line string) {
	if len(r.Residue) >= MaxResidueSpans {
		return
	}
	n := 0
	for _, s := range r.Residue {
		n += len(s) + 1
	}
	if n+len(line) > MaxResidueBytes {
		return
	}
	r.Residue = append(r.Residue, line)
}

var (
	sigID    = regexp.MustCompile(`^[A-Z]{3}-\d{3}$`)
	charName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} '.\-_]{1,36}$`)
)

// lines splits a paste into its non-empty lines, trimmed, with the
// carriage returns Windows clipboards add removed.
func lines(raw string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(raw, "\r", ""), "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Detect tells which paste raw is by what most of its lines look
// like, or "" when it is none of them.
func Detect(raw string) Kind {
	ls := lines(raw)
	if len(ls) == 0 {
		return ""
	}
	var dscan, probe, fleet, local int
	for _, l := range ls {
		cols := strings.Split(l, "\t")
		switch {
		case sigID.MatchString(strings.TrimSpace(cols[0])):
			probe++
		case len(cols) >= 4 && isInt(cols[0]):
			dscan++
		case len(cols) >= 5:
			fleet++
		case len(cols) == 1 && charName.MatchString(l):
			local++
		}
	}
	best, kind := 0, Kind("")
	for _, c := range []struct {
		n int
		k Kind
	}{{dscan, KindDScan}, {probe, KindProbe}, {fleet, KindFleet}, {local, KindLocal}} {
		if c.n > best {
			best, kind = c.n, c.k
		}
	}
	// Most lines must agree; a stray line or two is forgiven.
	if best*2 < len(ls) {
		return ""
	}
	return kind
}

// Parse reads raw as whichever paste it is.
func Parse(raw string) (Result, error) {
	if len(raw) > MaxBytes {
		return Result{}, ErrTooBig
	}
	ls := lines(raw)
	if len(ls) == 0 {
		return Result{}, ErrEmpty
	}
	if len(ls) > MaxLines {
		return Result{}, ErrTooBig
	}
	r := Result{Kind: Detect(raw)}
	switch r.Kind {
	case KindDScan:
		for _, l := range ls {
			c := strings.Split(l, "\t")
			if len(c) < 4 || !isInt(c[0]) {
				r.Skipped++
				r.addResidue(l)
				continue
			}
			id, _ := strconv.ParseInt(strings.TrimSpace(c[0]), 10, 64)
			r.DScan = append(r.DScan, DScanRow{TypeID: id, Name: strings.TrimSpace(c[1]), TypeName: strings.TrimSpace(c[2]), DistKm: ParseDistance(c[3])})
		}
	case KindProbe:
		for _, l := range ls {
			c := strings.Split(l, "\t")
			if !sigID.MatchString(strings.TrimSpace(c[0])) {
				r.Skipped++
				r.addResidue(l)
				continue
			}
			for len(c) < 6 {
				c = append(c, "")
			}
			r.Probe = append(r.Probe, ProbeRow{
				ID: strings.TrimSpace(c[0]), Group: strings.TrimSpace(c[1]), Type: strings.TrimSpace(c[2]),
				Name: strings.TrimSpace(c[3]), Signal: parsePercent(c[4]), DistKm: ParseDistance(c[5]),
			})
		}
	case KindFleet:
		for _, l := range ls {
			c := strings.Split(l, "\t")
			if len(c) < 5 {
				r.Skipped++
				r.addResidue(l)
				continue
			}
			f := FleetRow{Name: strings.TrimSpace(c[0]), System: strings.TrimSpace(c[1]), Ship: strings.TrimSpace(c[2]),
				Class: strings.TrimSpace(c[3]), Position: strings.TrimSpace(c[4])}
			if len(c) >= 7 {
				f.Wing = strings.TrimSpace(c[6])
			}
			r.Fleet = append(r.Fleet, f)
		}
	case KindLocal:
		seen := map[string]bool{}
		for _, l := range ls {
			if !charName.MatchString(l) {
				r.Skipped++
				r.addResidue(l)
				continue
			}
			k := strings.ToLower(l)
			if seen[k] {
				continue
			}
			seen[k] = true
			if len(r.Local) == MaxLocal {
				r.Skipped++
				continue
			}
			r.Local = append(r.Local, l)
		}
	default:
		return Result{}, ErrUnknown
	}
	return r, nil
}

func isInt(s string) bool {
	_, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return err == nil
}

// ParseDistance reads a distance as the client prints it, in any
// of its locales ("1,234 km", "1 234 km", "12 m", "3.2 AU",
// "3,2 AU"), as kilometres; -1 for "-" or anything unreadable.
func ParseDistance(s string) float64 {
	s = strings.TrimSpace(strings.ReplaceAll(s, " ", " "))
	if s == "" || s == "-" {
		return -1
	}
	f := strings.Fields(s)
	if len(f) < 2 {
		return -1
	}
	unit := strings.ToLower(f[len(f)-1])
	num := strings.Join(f[:len(f)-1], "")
	var mult float64
	decimal := false
	switch unit {
	case "m":
		mult = 0.001
	case "km":
		mult = 1
	case "au":
		mult, decimal = AUKm, true
	default:
		return -1
	}
	v, ok := parseNumber(num, decimal)
	if !ok {
		return -1
	}
	return v * mult
}

// parseNumber reads a localized number. With decimal, the last ','
// or '.' is the decimal point and the rest group thousands; without
// it (metres and kilometres, which the client prints whole) every
// separator groups thousands.
func parseNumber(s string, decimal bool) (float64, bool) {
	if s == "" {
		return 0, false
	}
	if decimal {
		at := strings.LastIndexAny(s, ",.")
		if at >= 0 {
			s = strings.NewReplacer(",", "", ".", "").Replace(s[:at]) + "." + s[at+1:]
		}
	} else {
		s = strings.NewReplacer(",", "", ".", "", "'", "").Replace(s)
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

func parsePercent(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	v, ok := parseNumber(s, true)
	if !ok {
		return 0
	}
	return v
}

// SystemFromCelestial reads the system name off a celestial's name
// as d-scan shows it: "Jita IV - Moon 4" and "Jita IV" give "Jita",
// "Jita - Star" gives "Jita"; "" when the name is no celestial's.
func SystemFromCelestial(name string) string {
	if i := strings.Index(name, " - Star"); i > 0 {
		return name[:i]
	}
	if m := celestialRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

var celestialRe = regexp.MustCompile(`^(.+?) (?:[IVXL]+)(?: - .*)?$`)
