package markethistory

import "strconv"

// ISKFloorChoice is one option in a daily ISK-traded filter select.
type ISKFloorChoice struct {
	Value  string
	Label  string
	Active bool
}

var iskFloorPresets = []ISKFloorChoice{
	{Value: "auto", Label: "Hub default (100M Forge, 20M other hubs)"},
	{Value: "0", Label: "Any (0 ISK)"},
	{Value: "5000000", Label: "5M ISK"},
	{Value: "20000000", Label: "20M ISK"},
	{Value: "50000000", Label: "50M ISK"},
	{Value: "100000000", Label: "100M ISK"},
	{Value: "250000000", Label: "250M ISK"},
	{Value: "500000000", Label: "500M ISK"},
	{Value: "1000000000", Label: "1B ISK"},
}

// ResolveMinISK parses a query parameter ("", "auto", "0", or a positive ISK
// amount) into a daily ISK-traded floor and its normalized select value.
func ResolveMinISK(raw string, regionID int64) (int64, string) {
	if raw == "" || raw == "auto" {
		return DefaultMinISK(regionID), "auto"
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
		return n, strconv.FormatInt(n, 10)
	}
	return DefaultMinISK(regionID), "auto"
}

// ISKFloorChoices returns the daily ISK-traded select options with selected
// marked active. If selected is a valid custom numeric floor not in the preset
// list, it is appended so the form echoes the active query value.
func ISKFloorChoices(selected string) []ISKFloorChoice {
	out := make([]ISKFloorChoice, 0, len(iskFloorPresets)+1)
	matched := false
	for _, c := range iskFloorPresets {
		active := c.Value == selected
		if active {
			matched = true
		}
		out = append(out, ISKFloorChoice{Value: c.Value, Label: c.Label, Active: active})
	}
	if !matched && selected != "" {
		out = append(out, ISKFloorChoice{Value: selected, Label: selected + " ISK", Active: true})
	}
	return out
}
