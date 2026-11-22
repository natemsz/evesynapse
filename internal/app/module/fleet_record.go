package module

import "fmt"

// FleetLeadersPath is the public fleet leaderboard.
const FleetLeadersPath = "/fleets/leaderboard/"

// FleetLeadersFor is one owner's all-time board on the fleet leaderboard,
// the span a fleet record states, named by its owner key.
func FleetLeadersFor(ownerKey string) string {
	return FleetLeadersPath + "?period=all&owner=" + ownerKey
}

// FleetRecordText says where a pilot or group stands on the public
// fleet leaderboard, in one sentence.
func FleetRecordText(place int32, fleets, kills, deaths, points int64) string {
	return fmt.Sprintf("Number %d on the fleet leaderboard, with %s, %s, %s and %s.", place,
		countOf(fleets, "fleet"), countOf(kills, "kill"), countOf(deaths, "death"), countOf(points, "point"))
}

func countOf(n int64, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
