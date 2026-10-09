package app

import (
	"strconv"
	"strings"

	"evesynapse/internal/discord"
	"evesynapse/internal/logging"
)

// discordIDValue reads a setting that holds one Discord id. Something
// that is not an id is said in the log and treated as not set: an id
// is used to give a role or post a message, and a wrong one must not
// be guessed at.
func discordIDValue(name, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !discord.ValidID(raw) {
		logging.Errorf("evesynapse: %s=%q is not a Discord id; ignored", name, raw)
		return ""
	}
	return raw
}

// discordIDMap reads a setting of the form
//
//	98000001=123456789012345678,98000002=234567890123456789
//
// corporation id = Discord id (a channel or a role, depending on the
// setting). "*" in place of a corporation id names the one used for
// every corporation not listed, kept under 0. An entry that cannot be
// read is said in the log and left out; the others still count.
func discordIDMap(name, raw string) map[int64]string {
	out := map[int64]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, found := strings.Cut(part, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		var corp int64
		ok := found && discord.ValidID(value)
		if ok && key != "*" {
			n, err := strconv.ParseInt(key, 10, 64)
			corp, ok = n, err == nil && n > 0
		}
		if !ok {
			logging.Errorf("evesynapse: %s: %q is not corporation=id; ignored", name, part)
			continue
		}
		out[corp] = value
	}
	return out
}
