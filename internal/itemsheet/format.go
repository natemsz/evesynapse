package itemsheet

import (
	"math"
	"strconv"
	"strings"
)

// Unit ids from the dump's eveUnits table that need more than a label.
const (
	unitMeter            = 1
	unitMass             = 2
	unitSecond           = 3
	unitVolume           = 9
	unitSpeed            = 10
	unitAcceleration     = 11
	unitMilliseconds     = 101
	unitMultiplier       = 104
	unitPercent          = 105
	unitInversePercent   = 108 // resonance: 0.4 is 60% resistance
	unitModifierPercent  = 109 // 1.1 is +10%
	unitInverseModifier  = 111 // 0.895 is 10.5%
	unitGroupID          = 115
	unitTypeID           = 116
	unitSizeClass        = 117
	unitAttributeID      = 119
	unitRealPercent      = 121
	unitFittingSlots     = 122
	unitRelativePercent  = 124
	unitAbsolutePercent  = 127
	unitBandwidth        = 128
	unitSlot             = 136
	unitBoolean          = 137
	unitBonus            = 139
	unitLevel            = 140
	unitHardpoints       = 141
	unitSex              = 142
	unitDatetime         = 143
	unitWarpSpeed        = 144
	unitModifierRealPerc = 205
)

// metersPerAU is one astronomical unit, for scan ranges.
const metersPerAU = 149_597_870_700

// FormatValue renders one attribute value the way the game shows it:
// milliseconds as seconds, long distances in km or AU, resonances as
// resistances, multipliers as signed percentages. unitName is the
// dump's label for unitID; it is used as is for units with no rule.
// Type and group ids (units 116 and 115) are resolved to names by the
// caller and never reach this function.
func FormatValue(unitID int64, unitName string, v float64) string {
	switch unitID {
	case unitMilliseconds:
		return Number(v/1000) + " s"
	case unitSecond:
		return Number(v) + " s"
	case unitMeter:
		if math.Abs(v) >= metersPerAU/10 {
			return Number(v/metersPerAU) + " AU"
		}
		if math.Abs(v) >= 10000 {
			return Number(v/1000) + " km"
		}
		return Number(v) + " m"
	case unitMass:
		return Number(v) + " kg"
	case unitVolume:
		return Number(v) + " m3"
	case unitSpeed, unitAcceleration:
		return Number(v) + " m/s"
	case unitBandwidth:
		return Number(v) + " Mbit/s"
	case unitWarpSpeed:
		return Number(v) + " AU/s"
	case unitInversePercent, unitInverseModifier:
		return Number((1-v)*100) + "%"
	case unitModifierPercent:
		return Signed((v-1)*100) + "%"
	case unitMultiplier:
		return Number(v) + "x"
	case unitPercent, unitRealPercent:
		return Number(v) + "%"
	case unitRelativePercent, unitModifierRealPerc:
		return Signed(v) + "%"
	case unitAbsolutePercent:
		return Number(v*100) + "%"
	case unitBonus:
		return Signed(v)
	case unitLevel:
		return "Level " + Number(v)
	case unitSizeClass:
		switch v {
		case 1:
			return "Small"
		case 2:
			return "Medium"
		case 3:
			return "Large"
		case 4:
			return "Capital"
		}
		return Number(v)
	case unitBoolean:
		if v != 0 {
			return "Yes"
		}
		return "No"
	case unitFittingSlots, unitHardpoints, unitSlot, unitAttributeID, unitSex, unitDatetime, unitGroupID, unitTypeID:
		return Number(v)
	}
	if unitName == "" {
		return Number(v)
	}
	return Number(v) + " " + unitName
}

// Number formats v with thousands separators and at most two
// decimals (three under 1, so 0.075 keeps its last digit), with
// trailing zeros dropped.
func Number(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0"
	}
	decimals := 2
	if a := math.Abs(v); a != 0 && a < 1 {
		decimals = 3
	}
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		s = "0"
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if frac != "" {
		b.WriteByte('.')
		b.WriteString(frac)
	}
	return b.String()
}

// Signed is Number with a plus sign on positive values.
func Signed(v float64) string {
	s := Number(v)
	if v > 0 && s != "0" {
		return "+" + s
	}
	return s
}
