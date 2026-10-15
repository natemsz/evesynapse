package app

import "evesynapse/internal/esi"

// kindOwners names the module whose pages are the only reader of a stored
// snapshot kind. Core's worker fetches such a kind only where that module
// is built in: a build without the mail module neither asks players for
// the mail scopes nor spends ESI on their mail.
var kindOwners = map[string]string{
	esi.SnapMail:       "mail",
	esi.SnapMailLabels: "mail",
	esi.SnapMailLists:  "mail",
	esi.SnapCalendar:   "calendar",
	esi.SnapContacts:   "contacts",
	esi.SnapContracts:  "contracts",
}

// kindBuiltIn reports whether a snapshot kind's owner (if it has a module
// owner) is built in.
func kindBuiltIn(kind string) bool {
	owner, ok := kindOwners[kind]
	return !ok || moduleCompiledIn(owner)
}

// builtInKinds is kinds without the ones whose module is not built in.
func builtInKinds(kinds []string) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if kindBuiltIn(k) {
			out = append(out, k)
		}
	}
	return out
}
