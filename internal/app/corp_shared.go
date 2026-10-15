package app

import (
	"strconv"
	"strings"
)

// humanizeEnum renders an ESI snake_case enum readably
// ("market_escrow" -> "market escrow").
func humanizeEnum(s string) string {
	return strings.ReplaceAll(s, "_", " ")
}

// maxLedgerRows caps the journal/transaction tables; the snapshot
// holds the newest page (up to 1,000 entries) but a page wants
// headlines, not a ledger dump.
const maxLedgerRows = 100

func idString(id int64) string { return strconv.FormatInt(id, 10) }

// srpPolicyMax is the longest text a corporation's SRP policy can be (the
// Ship Replacement module's own limit repeats it).
const srpPolicyMax = 2000
