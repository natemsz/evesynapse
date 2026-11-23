package app

// The demo account's fleet group (demo_seed.go has the seed itself): a
// private wormhole community with both demo pilots in it, a nano shield
// strategic cruiser doctrine, a history of fleets with battle reports,
// and one fleet that is always live with Sable as its boss.
//
// Everything is invented and stays out of shared state. The group is
// unlisted and its fleets are private, so nothing reaches a public
// board. Its killmails exist only as report rows, never in the shared
// kills table, so the public killboard never shows them. The reports go
// through the real merge (fleet_merge.go) from logs made up here.
//
// The live fleet never reads ESI. keepDemoFleetLive moves its clock
// along, and the Composition tab's commander tools work from what the
// visitor's page sends (handleFleetTry), so one visitor's changes are
// never stored and never reach another.

import (
	"context"
	"database/sql"
	"fmt"
	json "github.com/goccy/go-json"
	"math"
	"math/rand"
	"net/http"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
	"evesynapse/internal/gamelog"
	"evesynapse/internal/logging"
)

const (
	demoCommunityID   = int64(2900002001)
	demoCommunityName = "Deep Signal"
	demoCommunitySlug = "deep-signal-2900002001"

	demoCorpStaticID = int64(2900001004) // Quiet Static, in the group
	demoCorpSurveyID = int64(2900001005) // Far Survey Group, in the group
	demoCorpTideID   = int64(2900001006) // Hollow Tide, the rival group

	demoFleetMailBase = int64(2900008000)

	demoNanoDoctrine = "Nano Shield T3s"
	// demoLiveFleetAge is how long ago the live fleet always started.
	demoLiveFleetAge = 38 * time.Minute
)

// The group's own pilots and the rivals they fight.
const (
	demoPilotAiko = int64(2900000201 + iota)
	demoPilotBram
	demoPilotCass
	demoPilotDov
	demoPilotElin
	demoPilotFen
	demoPilotGreta
	demoPilotHollis
	demoPilotImre
	demoPilotJae
	demoPilotKira
	demoPilotLars
	demoPilotMaren
	demoPilotNils
	demoPilotOda
	demoPilotPriya
	demoPilotQuinn
	demoPilotRosa
	demoPilotSoren
	demoPilotTova
	demoPilotIvo
	demoRivalUlric
	demoRivalVera
	demoRivalWes
	demoRivalXan
	demoRivalYara
	demoRivalZeke
)

// demoFleetCast is everyone the group's fleets name beyond the seed's
// main cast. The rivals come last and are not members.
var demoFleetCast = []struct {
	ID     int64
	Name   string
	Corp   int64
	Joined string
}{
	{demoPilotAiko, "Aiko Renn", demoCorpStaticID, "2020-03-02T18:10:00Z"},
	{demoPilotBram, "Bram Tollan", demoCorpStaticID, "2019-07-21T20:42:00Z"},
	{demoPilotCass, "Cass Medina", demoCorpStaticID, "2022-01-09T15:03:00Z"},
	{demoPilotDov, "Dov Harker", demoCorpStaticID, "2021-05-30T02:18:00Z"},
	{demoPilotElin, "Elin Sato", demoCorpStaticID, "2023-02-14T11:27:00Z"},
	{demoPilotFen, "Fen Rourke", demoCorpStaticID, "2018-10-05T22:51:00Z"},
	{demoPilotGreta, "Greta Holt", demoCorpStaticID, "2020-12-12T07:36:00Z"},
	{demoPilotHollis, "Hollis Ward", demoCorpStaticID, "2021-09-17T19:14:00Z"},
	{demoPilotImre, "Imre Castell", demoCorpSurveyID, "2019-03-26T13:49:00Z"},
	{demoPilotJae, "Jae Moreau", demoCorpSurveyID, "2022-06-08T23:05:00Z"},
	{demoPilotKira, "Kira Nolan", demoCorpSurveyID, "2018-04-19T09:32:00Z"},
	{demoPilotLars, "Lars Ebner", demoCorpSurveyID, "2020-08-01T16:44:00Z"},
	{demoPilotMaren, "Maren Cole", demoCorpSurveyID, "2021-11-23T05:58:00Z"},
	{demoPilotNils, "Nils Arden", demoCorpSurveyID, "2023-04-02T21:12:00Z"},
	{demoPilotOda, "Oda Frey", demoCorpSurveyID, "2019-12-29T12:20:00Z"},
	{demoPilotPriya, "Priya Anand", demoCorpSurveyID, "2022-09-11T18:37:00Z"},
	{demoPilotQuinn, "Quinn Adair", demoCorpStaticID, "2020-05-16T03:41:00Z"},
	{demoPilotRosa, "Rosa Keel", demoCorpStaticID, "2021-02-07T14:09:00Z"},
	{demoPilotSoren, "Soren Vale", demoCorpSurveyID, "2023-07-25T20:26:00Z"},
	{demoPilotTova, "Tova Lind", demoCorpSurveyID, "2022-03-03T08:15:00Z"},
	{demoPilotIvo, "Ivo Brandt", demoCorpStaticID, "2021-06-19T17:53:00Z"},
	{demoRivalUlric, "Ulric Stone", demoCorpTideID, "2019-02-11T10:00:00Z"},
	{demoRivalVera, "Vera Pike", demoCorpTideID, "2020-07-04T22:30:00Z"},
	{demoRivalWes, "Wes Tarrant", demoCorpTideID, "2021-10-13T06:45:00Z"},
	{demoRivalXan, "Xan Mirov", demoCorpTideID, "2018-08-27T19:20:00Z"},
	{demoRivalYara, "Yara Flint", demoCorpTideID, "2022-11-30T01:10:00Z"},
	{demoRivalZeke, "Zeke Harlan", demoCorpTideID, "2020-01-22T15:55:00Z"},
}

// demoFleetSeat is one pilot's place in the fleet. Wing and squad are
// -1 above that level, as ESI reports them. System indexes the fleet's
// systems: the home hole first, then the chain.
type demoFleetSeat struct {
	ID         int64
	Ship       string
	Wing       int64
	Squad      int64
	Position   string
	System     int
	FleetRole  string // a commander's assigned role, if any
	FleetNotes []string
}

// demoFleetPool is the fleet, most reliable pilots first: a smaller
// past fleet is the front of this list.
var demoFleetPool = []demoFleetSeat{
	{ID: demoCharacterID, Ship: "Tengu", Wing: -1, Squad: -1, Position: "fleet_commander"},
	{ID: demoCharSera, Ship: "Loki", Wing: 1, Squad: -1, Position: "wing_commander"},
	{ID: demoCharDario, Ship: "Scimitar", Wing: 2, Squad: -1, Position: "wing_commander", FleetNotes: []string{"logi anchor"}},
	{ID: demoPilotAiko, Ship: "Tengu", Wing: 1, Squad: 11, Position: "squad_commander", FleetNotes: []string{"anchor"}},
	{ID: demoPilotKira, Ship: "Basilisk", Wing: 2, Squad: 21, Position: "squad_commander"},
	{ID: demoPilotBram, Ship: "Tengu", Wing: 1, Squad: 11, Position: "squad_member"},
	{ID: demoPilotOda, Ship: "Sabre", Wing: 3, Squad: 31, Position: "squad_commander", FleetNotes: []string{"bubbles"}},
	{ID: demoPilotLars, Ship: "Scimitar", Wing: 2, Squad: 21, Position: "squad_member"},
	{ID: demoPilotCass, Ship: "Tengu", Wing: 1, Squad: 11, Position: "squad_member"},
	{ID: demoPilotFen, Ship: "Tengu", Wing: 1, Squad: 12, Position: "squad_commander"},
	{ID: demoCharMina, Ship: "Claymore", Wing: 3, Squad: -1, Position: "wing_commander", FleetNotes: []string{"links"}},
	{ID: demoPilotGreta, Ship: "Loki", Wing: 1, Squad: 12, Position: "squad_member"},
	{ID: demoCharacter2ID, Ship: "Buzzard", Wing: 3, Squad: 32, Position: "squad_commander", System: 1, FleetNotes: []string{"chain scout"}},
	{ID: demoPilotMaren, Ship: "Scimitar", Wing: 2, Squad: 21, Position: "squad_member"},
	{ID: demoPilotDov, Ship: "Tengu", Wing: 1, Squad: 11, Position: "squad_member"},
	{ID: demoPilotQuinn, Ship: "Huginn", Wing: 3, Squad: 31, Position: "squad_member", FleetNotes: []string{"webs"}},
	{ID: demoPilotHollis, Ship: "Tengu", Wing: 1, Squad: 12, Position: "squad_member"},
	{ID: demoPilotPriya, Ship: "Sabre", Wing: 3, Squad: 31, Position: "squad_member"},
	{ID: demoPilotElin, Ship: "Loki", Wing: 1, Squad: 11, Position: "squad_member"},
	{ID: demoPilotNils, Ship: "Basilisk", Wing: 2, Squad: 21, Position: "squad_member", System: 3},
	{ID: demoPilotImre, Ship: "Tengu", Wing: 1, Squad: 12, Position: "squad_member"},
	{ID: demoPilotTova, Ship: "Buzzard", Wing: 3, Squad: 32, Position: "squad_member", System: 2},
	{ID: demoPilotJae, Ship: "Loki", Wing: 1, Squad: 12, Position: "squad_member"},
	{ID: demoPilotSoren, Ship: "Gila", Wing: 3, Squad: 31, Position: "squad_member"},
}

// demoLiveSeats is the fleet that is always running: Sable as fleet
// commander and boss, with ten of the group's pilots under her in two
// wings. One flies a hull the doctrine does not list and one scouts a
// jump ahead, so the commander tools have something to point out.
var demoLiveSeats = []demoFleetSeat{
	{ID: demoCharacterID, Ship: "Tengu", Wing: -1, Squad: -1, Position: "fleet_commander"},
	{ID: demoPilotAiko, Ship: "Tengu", Wing: 1, Squad: 11, Position: "squad_commander", FleetNotes: []string{"anchor"}},
	{ID: demoPilotBram, Ship: "Tengu", Wing: 1, Squad: 11, Position: "squad_member"},
	{ID: demoPilotCass, Ship: "Tengu", Wing: 1, Squad: 11, Position: "squad_member"},
	{ID: demoPilotDov, Ship: "Loki", Wing: 1, Squad: 11, Position: "squad_member"},
	{ID: demoPilotSoren, Ship: "Gila", Wing: 1, Squad: 11, Position: "squad_member"},
	{ID: demoPilotKira, Ship: "Basilisk", Wing: 2, Squad: 21, Position: "squad_commander", FleetNotes: []string{"logi anchor"}},
	{ID: demoPilotLars, Ship: "Scimitar", Wing: 2, Squad: 21, Position: "squad_member"},
	{ID: demoPilotOda, Ship: "Sabre", Wing: 2, Squad: 22, Position: "squad_commander", FleetNotes: []string{"bubbles"}},
	{ID: demoPilotQuinn, Ship: "Huginn", Wing: 2, Squad: 22, Position: "squad_member", FleetNotes: []string{"webs"}},
	{ID: demoPilotTova, Ship: "Buzzard", Wing: 2, Squad: 22, Position: "squad_member", System: 1, FleetNotes: []string{"chain scout"}},
}

// demoLiveUnits names the live fleet's wings and squads, as its boss
// would in game.
var demoLiveUnits = fleetUnitNames{
	Wings:  map[int64]string{1: "Main body", 2: "Support"},
	Squads: map[int64]string{11: "DPS", 21: "Logistics", 22: "Tackle and scouts"},
}

// demoInventedPilot says a character is one of the demo group's
// invented pilots or rivals. They have no public record, so their
// names are never links.
func demoInventedPilot(id int64) bool {
	return id >= demoPilotAiko && id <= demoRivalZeke
}

// demoFleetValues is what each hull is worth on a report, fitted.
var demoFleetValues = map[string]float64{
	"Tengu": 512e6, "Loki": 548e6, "Legion": 462e6, "Proteus": 603e6, "Scimitar": 268e6, "Basilisk": 274e6,
	"Guardian": 259e6, "Sabre": 82e6, "Huginn": 291e6, "Gila": 336e6, "Muninn": 284e6, "Cerberus": 247e6,
	"Ishtar": 271e6, "Machariel": 612e6, "Nightmare": 743e6, "Vargur": 2.14e9, "Bhaalgorn": 1.18e9,
	"Stiletto": 47e6, "Buzzard": 43e6, "Claymore": 362e6, "Falcon": 296e6, "Zealot": 233e6,
}

// demoFleetMail is one killmail of a past battle. A kill names the
// rival who died; a loss names a seat in the fleet.
type demoFleetMail struct {
	Ship   string
	Victim int64
	Loss   bool
	Seat   int
	At     time.Duration // into the battle
}

type demoFleetBattle struct {
	System int // index into the fleet's systems
	Offset time.Duration
	Length time.Duration
	Mails  []demoFleetMail
}

type demoPastFleet struct {
	Title, Description string
	Ago                time.Duration
	Minutes            int64
	FC                 int64
	Pilots             int
	Logs               int
	Battles            []demoFleetBattle
}

func demoKill(ship string, victim int64, at time.Duration) demoFleetMail {
	return demoFleetMail{Ship: ship, Victim: victim, At: at}
}

func demoLoss(seat int, at time.Duration) demoFleetMail {
	return demoFleetMail{Loss: true, Seat: seat, At: at}
}

func demoPastFleets() []demoPastFleet {
	const day, minute, second = 24 * time.Hour, time.Minute, time.Second
	return []demoPastFleet{
		{
			Title: "Chain patrol", Description: "Clear the chain before the static rolls. Scouts out first.",
			Ago: 26 * time.Hour, Minutes: 110, FC: demoCharacterID, Pilots: 16, Logs: 11,
			Battles: []demoFleetBattle{{System: 1, Offset: 24 * minute, Length: 6 * minute, Mails: []demoFleetMail{
				demoKill("Proteus", demoRivalUlric, 95*second), demoKill("Gila", demoRivalVera, 170*second),
				demoLoss(8, 215*second), demoKill("Scimitar", demoRivalWes, 290*second),
			}}},
		},
		{
			Title: "Home defense: Hollow Tide probe", Description: "Hollow Tide rolled into our static. Everyone in doctrine, form on the citadel.",
			Ago: 3*day + 2*time.Hour, Minutes: 150, FC: demoCharSera, Pilots: 22, Logs: 15,
			Battles: []demoFleetBattle{
				{System: 0, Offset: 31 * minute, Length: 9 * minute, Mails: []demoFleetMail{
					demoKill("Sabre", demoRivalXan, 60*second), demoKill("Guardian", demoRivalYara, 150*second),
					demoKill("Legion", demoRivalUlric, 240*second), demoLoss(11, 300*second),
					demoKill("Guardian", demoRivalVera, 360*second), demoKill("Legion", demoRivalZeke, 480*second),
				}},
				{System: 1, Offset: 74 * minute, Length: 5 * minute, Mails: []demoFleetMail{
					demoKill("Muninn", demoRivalWes, 75*second), demoLoss(14, 150*second), demoKill("Buzzard", demoRivalXan, 240*second),
				}},
			},
		},
		{
			Title: "Thera run", Description: "Quick roam through Thera for targets of opportunity. Bring a spare clone.",
			Ago: 6*day + 5*time.Hour, Minutes: 90, FC: demoCharDario, Pilots: 12, Logs: 6,
			Battles: []demoFleetBattle{{System: 3, Offset: 41 * minute, Length: 4 * minute, Mails: []demoFleetMail{
				demoKill("Stiletto", demoRivalYara, 50*second), demoKill("Zealot", demoRivalZeke, 170*second),
			}}},
		},
		{
			Title: "C5 site escort", Description: "Guard the site runners while they clear the static. Quiet night expected.",
			Ago: 9*day + 3*time.Hour, Minutes: 75, FC: demoCharSera, Pilots: 8,
		},
		{
			Title: "Eviction counterattack", Description: "Allies called it in. Full doctrine, all logi, strict anchor discipline.",
			Ago: 13*day + 4*time.Hour, Minutes: 200, FC: demoCharacterID, Pilots: 24, Logs: 19,
			Battles: []demoFleetBattle{
				{System: 2, Offset: 38 * minute, Length: 11 * minute, Mails: []demoFleetMail{
					demoKill("Falcon", demoRivalVera, 70*second), demoKill("Guardian", demoRivalWes, 160*second),
					demoKill("Machariel", demoRivalUlric, 280*second), demoLoss(5, 340*second),
					demoKill("Nightmare", demoRivalZeke, 430*second), demoKill("Guardian", demoRivalYara, 520*second),
					demoLoss(17, 570*second), demoKill("Bhaalgorn", demoRivalXan, 630*second),
				}},
				{System: 2, Offset: 96 * minute, Length: 7 * minute, Mails: []demoFleetMail{
					demoKill("Vargur", demoRivalUlric, 200*second), demoKill("Cerberus", demoRivalVera, 310*second), demoLoss(9, 360*second),
				}},
				{System: 0, Offset: 151 * minute, Length: 3 * minute, Mails: []demoFleetMail{
					demoKill("Ishtar", demoRivalWes, 90*second),
				}},
			},
		},
		{
			Title: "Weekend roam", Description: "Low stakes roam out the static. New pilots welcome, ask for a fit.",
			Ago: 20*day + 6*time.Hour, Minutes: 100, FC: demoCharDario, Pilots: 14, Logs: 7,
			Battles: []demoFleetBattle{{System: 1, Offset: 52 * minute, Length: 5 * minute, Mails: []demoFleetMail{
				demoKill("Cerberus", demoRivalZeke, 80*second), demoLoss(12, 190*second), demoKill("Huginn", demoRivalYara, 250*second),
			}}},
		},
	}
}

// demoFleetTypeNames is every hull and module the group's fits and
// fleets name.
var demoFleetTypeNames = []string{
	"Tengu", "Loki", "Legion", "Proteus", "Scimitar", "Basilisk", "Guardian", "Sabre", "Huginn", "Gila", "Muninn",
	"Cerberus", "Ishtar", "Machariel", "Nightmare", "Vargur", "Bhaalgorn", "Stiletto", "Buzzard", "Claymore", "Falcon", "Zealot",
	"Tengu Core - Electronic Efficiency Gate", "Tengu Defensive - Supplemental Screening",
	"Tengu Offensive - Accelerated Ejection Bay", "Tengu Propulsion - Chassis Optimization",
	"Loki Core - Immobility Drivers", "Loki Defensive - Adaptive Defense Node",
	"Loki Offensive - Projectile Scoping Array", "Loki Propulsion - Intercalated Nanofibers",
	"50MN Microwarpdrive II", "5MN Microwarpdrive II", "Large Shield Extender II", "Medium Shield Extender II",
	"Multispectrum Shield Hardener II", "Warp Disruptor II", "Stasis Webifier II", "Nanofiber Internal Structure II",
	"Ballistic Control System II", "Gyrostabilizer II", "Damage Control II", "Heavy Missile Launcher II",
	"425mm AutoCannon II", "200mm AutoCannon II", "Large Remote Shield Booster II", "Sensor Booster II",
	"Cap Recharger II", "Co-Processor II", "Medium Core Defense Field Extender I", "Interdiction Sphere Launcher I",
	"Shield Command Burst II", "Skirmish Command Burst II", "Covert Ops Cloaking Device II", "Core Probe Launcher II",
	"Data Analyzer II", "Relic Analyzer II", "Inertial Stabilizers II", "Multispectral ECM II", "Signal Distortion Amplifier II",
}

type demoDoctrineFit struct {
	Name, Ship, Role, Note string
	Items                  []fitview.FitDocItem
}

// demoNanoFits is the doctrine. Slot counts match each hull, and the
// strategic cruisers' with the subsystems listed. No pilot flies the
// Falcon, so the Composition tab has a real gap to show.
func (s *demoSeed) demoNanoFits() []demoDoctrineFit {
	t := s.typeID
	item := func(name string, qty int) fitview.FitDocItem { return fitview.FitDocItem{TypeID: t(name), Qty: qty} }
	return []demoDoctrineFit{
		{"Nano Tengu", "Tengu", "DPS", "Main damage. Keep range, stay aligned, follow the anchor.", []fitview.FitDocItem{
			item("Tengu Core - Electronic Efficiency Gate", 1), item("Tengu Defensive - Supplemental Screening", 1),
			item("Tengu Offensive - Accelerated Ejection Bay", 1), item("Tengu Propulsion - Chassis Optimization", 1),
			item("Heavy Missile Launcher II", 6),
			item("50MN Microwarpdrive II", 1), item("Large Shield Extender II", 2), item("Multispectrum Shield Hardener II", 1),
			item("Warp Disruptor II", 1), item("Stasis Webifier II", 1),
			item("Ballistic Control System II", 2), item("Nanofiber Internal Structure II", 1), item("Damage Control II", 1),
			item("Medium Core Defense Field Extender I", 3),
		}},
		{"Nano Loki", "Loki", "DPS", "Damage and heavy webs. Web whatever the FC calls for tackle.", []fitview.FitDocItem{
			item("Loki Core - Immobility Drivers", 1), item("Loki Defensive - Adaptive Defense Node", 1),
			item("Loki Offensive - Projectile Scoping Array", 1), item("Loki Propulsion - Intercalated Nanofibers", 1),
			item("425mm AutoCannon II", 5),
			item("50MN Microwarpdrive II", 1), item("Large Shield Extender II", 1), item("Multispectrum Shield Hardener II", 1), item("Stasis Webifier II", 2),
			item("Gyrostabilizer II", 2), item("Nanofiber Internal Structure II", 2), item("Damage Control II", 1),
			item("Medium Core Defense Field Extender I", 3),
		}},
		{"Shield Scimitar", "Scimitar", "Logistics", "Stay on the logi anchor. Broadcast for reps, never chase.", []fitview.FitDocItem{
			item("Large Remote Shield Booster II", 4),
			item("50MN Microwarpdrive II", 1), item("Large Shield Extender II", 2), item("Multispectrum Shield Hardener II", 1), item("Sensor Booster II", 1),
			item("Nanofiber Internal Structure II", 2), item("Damage Control II", 1), item("Co-Processor II", 1),
			item("Medium Core Defense Field Extender I", 2),
		}},
		{"Shield Basilisk", "Basilisk", "Logistics", "Cap chain with the other Basilisk, then shield reps.", []fitview.FitDocItem{
			item("Large Remote Shield Booster II", 4),
			item("50MN Microwarpdrive II", 1), item("Large Shield Extender II", 2), item("Multispectrum Shield Hardener II", 1), item("Cap Recharger II", 1),
			item("Damage Control II", 1), item("Nanofiber Internal Structure II", 1),
			item("Medium Core Defense Field Extender I", 2),
		}},
		{"Bubble Sabre", "Sabre", "Tackle", "First on grid. Bubble on the FC's call and burn out.", []fitview.FitDocItem{
			item("Interdiction Sphere Launcher I", 1), item("200mm AutoCannon II", 6),
			item("5MN Microwarpdrive II", 1), item("Warp Disruptor II", 1), item("Medium Shield Extender II", 2),
			item("Damage Control II", 1), item("Nanofiber Internal Structure II", 1),
		}},
		{"Web Huginn", "Huginn", "Tackle", "Long webs on anything trying to leave.", []fitview.FitDocItem{
			item("425mm AutoCannon II", 4),
			item("50MN Microwarpdrive II", 1), item("Large Shield Extender II", 1), item("Multispectrum Shield Hardener II", 1), item("Stasis Webifier II", 3),
			item("Gyrostabilizer II", 2), item("Nanofiber Internal Structure II", 1), item("Damage Control II", 1),
			item("Medium Core Defense Field Extender I", 2),
		}},
		{"Link Claymore", "Claymore", "Command", "Shield and skirmish links, on grid with the fleet.", []fitview.FitDocItem{
			item("Shield Command Burst II", 1), item("Skirmish Command Burst II", 1), item("Heavy Missile Launcher II", 5),
			item("50MN Microwarpdrive II", 1), item("Large Shield Extender II", 2), item("Multispectrum Shield Hardener II", 1), item("Warp Disruptor II", 1), item("Stasis Webifier II", 1),
			item("Ballistic Control System II", 2), item("Nanofiber Internal Structure II", 1), item("Damage Control II", 1),
			item("Medium Core Defense Field Extender I", 2),
		}},
		{"Scout Buzzard", "Buzzard", "Scout", "Scan the chain and call every new signature.", []fitview.FitDocItem{
			item("Covert Ops Cloaking Device II", 1), item("Core Probe Launcher II", 1),
			item("5MN Microwarpdrive II", 1), item("Data Analyzer II", 1), item("Relic Analyzer II", 1),
			item("Inertial Stabilizers II", 2), item("Nanofiber Internal Structure II", 1),
		}},
		{"ECM Falcon", "Falcon", "EWAR", "Jam enemy logi first. Cloak up if you get pointed.", []fitview.FitDocItem{
			item("Covert Ops Cloaking Device II", 1),
			item("50MN Microwarpdrive II", 1), item("Multispectral ECM II", 5), item("Sensor Booster II", 1),
			item("Signal Distortion Amplifier II", 2), item("Nanofiber Internal Structure II", 1),
		}},
	}
}

// isDemoFleet: this fleet belongs to the demo's group, so nothing reads
// it from ESI and the worker does not rebuild its report.
func isDemoFleet(op db.Op) bool {
	return op.OwnerKind == ownerCommunity && op.OwnerID == demoCommunityID
}

// keepDemoFleetLive moves the live demo fleet's clock along, so it is
// always running and freshly read. It writes one fleet's rows, and none
// on an install with no demo group.
func (app *Application) keepDemoFleetLive(ctx context.Context, now time.Time) {
	if app.demoUserID(ctx) == 0 {
		return
	}
	now = now.UTC()
	if err := app.queries.KeepDemoFleetLive(ctx, db.KeepDemoFleetLiveParams{
		StartsAt: now.Add(-demoLiveFleetAge), SeenAt: now, OwnerID: demoCommunityID,
	}); err != nil {
		logging.Errorf("demo: keep fleet live: %v", err)
	}
}

// demoFleetSystems is the group's wormhole chain, by name. A system this
// install's SDE lacks falls back to one the seed already holds.
func (s *demoSeed) demoFleetSystems(ctx context.Context) []int64 {
	out := make([]int64, 0, 4)
	for _, name := range []string{"J164430", "J121116", "J115405", "Thera"} {
		id := s.tamaSystemID
		if row, err := s.app.queries.GetSDESystemByName(ctx, name); err == nil {
			id = row.SystemID
		}
		out = append(out, id)
	}
	return out
}

// writeDemoFleets files the whole group.
func (s *demoSeed) writeDemoFleets(ctx context.Context) error {
	q := s.app.queries
	rows, err := q.ListSDETypesByNames(ctx, demoFleetTypeNames)
	if err != nil {
		return fmt.Errorf("demo seed: resolve fleet types: %w", err)
	}
	for _, r := range rows {
		if cur, ok := s.types[r.Name]; !ok || r.TypeID < cur {
			s.types[r.Name] = r.TypeID
		}
	}
	if err := s.writeDemoCommunity(ctx); err != nil {
		return err
	}
	doctrineID, err := s.writeDemoDoctrine(ctx)
	if err != nil {
		return err
	}
	policyID, err := q.CreateSRPPolicy(ctx, db.CreateSRPPolicyParams{
		OwnerKind: ownerCommunity, OwnerID: demoCommunityID, Name: "Doctrine losses", Mode: "percent_of_loss", Percent: 80,
		RequiresDoctrine: true, RequiresAttendance: true, ClaimWindowHours: 168, LedgerVisibility: "members",
		PolicyText: "Doctrine hulls lost on a group fleet are covered at 80 percent. Claim within a week of the fleet.",
		IsDefault:  true, UpdatedBy: demoCharacterID, UpdatedAt: s.now.Add(-60 * 24 * time.Hour),
	})
	if err != nil {
		return fmt.Errorf("demo seed: fleet SRP policy: %w", err)
	}
	systems := s.demoFleetSystems(ctx)
	mailID := demoFleetMailBase
	for i, f := range demoPastFleets() {
		if err := s.writeDemoPastFleet(ctx, f, i, doctrineID, policyID, systems, &mailID); err != nil {
			return fmt.Errorf("demo seed: fleet %q: %w", f.Title, err)
		}
	}
	if err := s.writeDemoLiveFleet(ctx, doctrineID, policyID, systems); err != nil {
		return fmt.Errorf("demo seed: live fleet: %w", err)
	}
	if err := s.writeDemoPlannedFleet(ctx, doctrineID, policyID); err != nil {
		return fmt.Errorf("demo seed: planned fleet: %w", err)
	}
	if err := s.app.refreshFleetLeaders(ctx, s.now); err != nil {
		return fmt.Errorf("demo seed: fleet leaderboards: %w", err)
	}
	return nil
}

// writeDemoCommunity files the group, its members and its teams. It is
// unlisted and takes members by invite, so only its members see it.
func (s *demoSeed) writeDemoCommunity(ctx context.Context) error {
	q := s.app.queries
	const day = 24 * time.Hour
	invite, err := newShareToken()
	if err != nil {
		return err
	}
	if err := q.InsertDemoCommunity(ctx, db.InsertDemoCommunityParams{
		ID: demoCommunityID, Slug: demoCommunitySlug, Name: demoCommunityName,
		Description: "A wormhole group. We fly nano shield strategic cruisers, keep the chain scouted, and split the loot evenly.",
		InviteToken: sql.NullString{String: invite, Valid: true}, CreatedByCharacter: demoCharacterID, CreatedAt: s.now.Add(-420 * day),
	}); err != nil {
		return fmt.Errorf("demo seed: community: %w", err)
	}
	for _, pilot := range demoPilots {
		if err := q.UpsertVerifiedPilot(ctx, db.UpsertVerifiedPilotParams{
			CharacterID: pilot.ID, Name: pilot.Name, CorporationID: demoCorpID,
			AffiliationAt: sql.NullTime{Time: s.now, Valid: true}, SeenAt: s.now,
		}); err != nil {
			return fmt.Errorf("demo seed: verified pilot: %w", err)
		}
	}
	names := map[int64]string{demoCharSera: "Sera Voss", demoCharDario: "Dario Fenn", demoCharMina: "Mina Okafor"}
	for _, pilot := range demoPilots {
		names[pilot.ID] = pilot.Name
	}
	for _, c := range demoFleetCast {
		names[c.ID] = c.Name
	}
	roles := map[int64]string{demoCharacterID: "leader", demoCharSera: "admin", demoCharDario: "admin"}
	members := []int64{demoCharacterID, demoCharacter2ID, demoCharSera, demoCharDario, demoCharMina}
	for _, c := range demoFleetCast {
		if c.Corp != demoCorpTideID {
			members = append(members, c.ID)
		}
	}
	for i, id := range members {
		role := roles[id]
		if role == "" {
			role = "member"
		}
		if _, err := q.AddCommunityMember(ctx, db.AddCommunityMemberParams{
			CommunityID: demoCommunityID, CharacterID: id, Name: names[id], Role: role, State: "active",
			JoinedAt: s.now.Add(-time.Duration(410-i*11) * day), AddedBy: demoCharacterID,
		}); err != nil {
			return fmt.Errorf("demo seed: community member: %w", err)
		}
	}
	for i, g := range []struct {
		name, description string
		members           []int64
	}{
		{"FC team", "Runs the fleets and keeps the doctrine current.", []int64{demoCharacterID, demoCharSera, demoCharDario}},
		{"Logistics wing", "Scimitar and Basilisk pilots. First ping when a fleet forms.", []int64{demoCharDario, demoPilotKira, demoPilotLars, demoPilotMaren, demoPilotNils}},
		{"Chain scouts", "Keep the chain mapped and call new signatures.", []int64{demoCharacter2ID, demoPilotTova, demoPilotQuinn}},
	} {
		groupID, err := q.CreateOrgGroup(ctx, db.CreateOrgGroupParams{
			OwnerKind: ownerCommunity, OwnerID: demoCommunityID, Name: g.name, Description: g.description,
			CreatedBy: demoCharacterID, CreatedAt: s.now.Add(-time.Duration(300-i*40) * day),
		})
		if err != nil {
			return fmt.Errorf("demo seed: community group: %w", err)
		}
		for j, member := range g.members {
			if err := q.AddOrgGroupMember(ctx, db.AddOrgGroupMemberParams{
				GroupID: groupID, CharacterID: member, AddedBy: demoCharacterID,
				AddedAt: s.now.Add(-time.Duration(290-i*40-j) * day),
			}); err != nil {
				return fmt.Errorf("demo seed: community group member: %w", err)
			}
		}
	}
	return nil
}

func (s *demoSeed) writeDemoDoctrine(ctx context.Context) (int64, error) {
	q := s.app.queries
	created := s.now.Add(-70 * 24 * time.Hour)
	doctrineID, err := q.CreateDoctrine(ctx, db.CreateDoctrineParams{
		OwnerKind: ownerCommunity, OwnerID: demoCommunityID, Visibility: module.DoctrineMembers,
		Name: demoNanoDoctrine, Category: "Wormhole", Tags: "wormhole,t3,nano,shield",
		Description: "Fast shield strategic cruisers with Scimitar and Basilisk logistics, heavy tackle, links and a scout on the chain.",
		CreatedBy:   demoCharacterID, CreatedAt: created,
	})
	if err != nil {
		return 0, fmt.Errorf("demo seed: fleet doctrine: %w", err)
	}
	for _, f := range s.demoNanoFits() {
		ship := s.typeID(f.Ship)
		if ship == 0 {
			continue
		}
		var kept []fitview.FitDocItem
		for _, it := range f.Items {
			if it.TypeID != 0 {
				kept = append(kept, it)
			}
		}
		raw, err := json.Marshal(fitview.FitDoc{Name: f.Name, ShipTypeID: ship, Items: kept, Charges: map[int64]int64{}})
		if err != nil {
			return 0, err
		}
		if _, err := q.AddDoctrineFit(ctx, db.AddDoctrineFitParams{
			DoctrineID: doctrineID, Name: f.Name, ShipTypeID: ship, ItemsJson: string(raw),
			FleetRole: f.Role, Note: f.Note, AddedBy: demoCharacterID, AddedAt: created,
		}); err != nil {
			return 0, fmt.Errorf("demo seed: fleet doctrine fit: %w", err)
		}
	}
	return doctrineID, nil
}

// createDemoFleet files one private fleet of the group.
func (s *demoSeed) createDemoFleet(ctx context.Context, title, description string, start time.Time, minutes, fc, doctrineID, policyID int64) (db.Op, error) {
	q := s.app.queries
	token, err := newShareToken()
	if err != nil {
		return db.Op{}, err
	}
	mode, doctrine := doctrineNone, ""
	if doctrineID != 0 {
		mode, doctrine = doctrineRequired, demoNanoDoctrine
	}
	id, err := q.CreateFleet(ctx, db.CreateFleetParams{
		OwnerKind: ownerCommunity, OwnerID: demoCommunityID, FleetType: "community",
		Visibility: fleetRestricted, JoinGate: joinByRule, ShareToken: sql.NullString{String: token, Valid: true},
		Title: title, Description: description, StartsAt: start, DurationMinutes: minutes,
		Doctrine: doctrine, DoctrineID: doctrineID, DoctrineMode: mode, FormUp: "Home citadel, J164430",
		FcCharacterID: fc, CreatedByCharacter: fc, CreatedAt: start.Add(-48 * time.Hour),
	})
	if err != nil {
		return db.Op{}, err
	}
	if err := q.SetOpSRPPolicy(ctx, db.SetOpSRPPolicyParams{ID: id, SrpPolicyID: sql.NullInt64{Int64: policyID, Valid: true}}); err != nil {
		return db.Op{}, err
	}
	return q.GetOp(ctx, id)
}

// demoSeatsParams is the attendance write for some seats.
func (s *demoSeed) demoSeatsParams(opID int64, seats []demoFleetSeat, systems []int64, seenAt time.Time) db.UpsertFleetAttendanceParams {
	p := db.UpsertFleetAttendanceParams{OpID: opID, SeenAt: seenAt}
	for _, seat := range seats {
		p.CharacterIds = append(p.CharacterIds, seat.ID)
		p.ShipTypeIds = append(p.ShipTypeIds, s.typeID(seat.Ship))
		p.WingIds = append(p.WingIds, seat.Wing)
		p.SquadIds = append(p.SquadIds, seat.Squad)
		p.Positions = append(p.Positions, seat.Position)
		p.SystemIds = append(p.SystemIds, systems[seat.System])
		p.SuggestedRoles = append(p.SuggestedRoles, "")
	}
	return p
}

// demoLogWork is a hull's damage out, damage in and repairs per second
// at the height of a fight.
func demoLogWork(ship string) (out, in, reps float64) {
	switch ship {
	case "Tengu":
		return 690, 240, 0
	case "Loki":
		return 610, 290, 0
	case "Scimitar", "Basilisk":
		return 25, 150, 1120
	case "Sabre":
		return 210, 320, 0
	case "Huginn":
		return 280, 170, 0
	case "Claymore":
		return 340, 180, 0
	case "Gila":
		return 520, 130, 0
	case "Buzzard":
		return 0, 20, 0
	}
	return 300, 150, 0
}

// demoLogRead makes up one pilot's log of one battle, cut on the clock
// steps a real log is stored in.
func demoLogRead(id int64, seat demoFleetSeat, mate int64, from time.Time, length time.Duration, system int64, rng *rand.Rand) *fleetLogRead {
	const stepSeconds = int64(gamelog.SliceSeconds)
	first := gamelog.ClockStep(from.Unix())
	last := gamelog.ClockStep(from.Add(length).Unix())
	out, in, reps := demoLogWork(seat.Ship)
	r := &fleetLogRead{
		ID: id, Character: seat.ID, Start: from, End: from.Add(length), Received: from.Add(length + time.Minute),
		SystemID: system, Usable: true, Fleetmates: map[int64]int64{1: mate},
	}
	phase := rng.Float64() * math.Pi
	total := float64(last-first) + float64(stepSeconds)
	for at := first; at <= last; at += stepSeconds {
		t := float64(at-first) / total
		shape := math.Sin(math.Pi*t) * (0.74 + 0.26*math.Sin(t*11+phase))
		swing := 0.82 + rng.Float64()*0.36
		work := func(perSecond float64) int64 {
			return int64(perSecond * float64(stepSeconds) * max(shape, 0.04) * swing)
		}
		rows := [][7]int64{{0, work(out), work(in * (1.5 - t)), work(reps), 0, 0, 0}}
		if mate != 0 && out > 100 && rng.Intn(14) == 0 {
			rows = append(rows, [7]int64{1, 40 + rng.Int63n(260), 0, 0, 0, 0, 0})
		}
		r.Steps = append(r.Steps, gamelog.AlignedSlice{At: at, Rows: rows})
	}
	return r
}

// writeDemoPastFleet files a closed fleet and its report, built by the
// real merge from made-up logs and mails.
func (s *demoSeed) writeDemoPastFleet(ctx context.Context, f demoPastFleet, index int, doctrineID, policyID int64, systems []int64, mailID *int64) error {
	q := s.app.queries
	start := s.now.Add(-f.Ago).Truncate(time.Minute)
	op, err := s.createDemoFleet(ctx, f.Title, f.Description, start, f.Minutes, f.FC, doctrineID, policyID)
	if err != nil {
		return err
	}
	seats := demoFleetPool[:min(f.Pilots, len(demoFleetPool))]
	end := start.Add(time.Duration(f.Minutes) * time.Minute)
	for _, seenAt := range []time.Time{start.Add(3 * time.Minute), end.Add(-4 * time.Minute)} {
		if _, err := q.UpsertFleetAttendance(ctx, s.demoSeatsParams(op.ID, seats, systems, seenAt)); err != nil {
			return err
		}
	}
	if err := q.SetOpCaptureStatus(ctx, db.SetOpCaptureStatusParams{
		CaptureStatus: "", CheckedAt: sql.NullTime{Time: end, Valid: true}, BossID: f.FC, Detail: "", ID: op.ID,
	}); err != nil {
		return err
	}
	attendance, err := q.ListOpAttendance(ctx, op.ID)
	if err != nil {
		return err
	}
	from, to := fleetReportWindow(op)
	on := fleetPresences(attendance, from, to)
	rng := rand.New(rand.NewSource(int64(index) + 41))
	ships := make(map[int64]int64, len(seats))
	for _, seat := range seats {
		ships[seat.ID] = s.typeID(seat.Ship)
	}

	var reads []*fleetLogRead
	var mails []*fleetMail
	readID := int64(1)
	for _, b := range f.Battles {
		system := systems[b.System]
		at := start.Add(b.Offset)
		for i, seat := range seats[:min(f.Logs, len(seats))] {
			mate := seats[(i+1)%len(seats)].ID
			reads = append(reads, demoLogRead(readID, seat, mate, at, b.Length, system, rng))
			readID++
		}
		for _, m := range b.Mails {
			*mailID++
			mail := &fleetMail{ID: *mailID, At: at.Add(m.At).UTC(), System: system, Counts: true}
			if m.Loss {
				seat := seats[m.Seat]
				mail.Loss, mail.Victim, mail.Ship = true, seat.ID, s.typeID(seat.Ship)
				mail.Damage = 28000 + rng.Int63n(30000)
				mail.Value = sql.NullFloat64{Float64: demoFleetValues[seat.Ship], Valid: true}
			} else {
				mail.Victim, mail.Ship = m.Victim, s.typeID(m.Ship)
				mail.Value = sql.NullFloat64{Float64: demoFleetValues[m.Ship], Valid: true}
				count := 4 + rng.Intn(max(len(seats)-4, 1))
				for _, pick := range rng.Perm(len(seats))[:min(count, len(seats))] {
					if out, _, _ := demoLogWork(seats[pick].Ship); out > 100 {
						damage := int64(out) * (4 + rng.Int63n(9))
						mail.Attackers = append(mail.Attackers, fleetMailAttacker{Character: seats[pick].ID, Ship: s.typeID(seats[pick].Ship), Damage: damage})
						mail.Damage += damage
					}
				}
			}
			mails = append(mails, mail)
		}
	}

	build := &fleetReportBuild{}
	counted := countedByRead(reads)
	for i, c := range clusterFleetBattles(mails, reads, on) {
		if b, ok := composeFleetBattle(op.ID, int32(i), c, counted, on, ships, s.now); ok {
			b.FightIDs = nil // made-up logs have no stored fight to link
			build.Battles = append(build.Battles, b)
		}
	}
	for i := range build.Battles {
		build.Battles[i].Battle.Seq = int32(i)
	}
	build.Pilots, build.Mails = fleetScoreRows(op, attendance, nil, mails)
	if err := q.MarkFleetReportDirty(ctx, op.ID); err != nil {
		return err
	}
	state, err := q.GetFleetReportState(ctx, op.ID)
	if err != nil {
		return err
	}
	if err := s.app.storeFleetReport(ctx, op.ID, state.DirtySeq.Int64, build, s.now); err != nil {
		return err
	}
	for _, seat := range seats[:min(f.Logs, len(seats))] {
		if seat.ID != demoCharacterID && seat.ID != demoCharacter2ID {
			continue
		}
		if err := q.SetFleetBattleConsent(ctx, db.SetFleetBattleConsentParams{
			OpID: op.ID, CharacterID: seat.ID, UserID: s.userID, ConsentedAt: end,
		}); err != nil {
			return err
		}
	}
	return s.writeDemoFleetClaims(ctx, op, policyID, mails, end)
}

// writeDemoFleetClaims files a ship replacement claim for each loss: the
// older ones paid, the newest still waiting on a handler.
func (s *demoSeed) writeDemoFleetClaims(ctx context.Context, op db.Op, policyID int64, mails []*fleetMail, end time.Time) error {
	q := s.app.queries
	for _, m := range mails {
		if !m.Loss {
			continue
		}
		userID := int64(0)
		if m.Victim == demoCharacterID || m.Victim == demoCharacter2ID {
			userID = s.userID
		}
		payout := m.Value.Float64 * 0.8
		id, err := q.CreateSRPRequest(ctx, db.CreateSRPRequestParams{
			OwnerKind: ownerCommunity, OwnerID: demoCommunityID, PolicyID: sql.NullInt64{Int64: policyID, Valid: true},
			OpID: op.ID, KillmailID: m.ID, KillmailHash: demoKillmailHash(m.ID), CharacterID: m.Victim, UserID: userID,
			ShipTypeID: m.Ship, SolarSystemID: m.System, LostAt: m.At, LossValue: m.Value.Float64, SuggestedPayout: payout,
			Note: "Lost on the fleet, in doctrine.", CreatedAt: end.Add(time.Hour),
		})
		if err != nil {
			return err
		}
		if s.now.Sub(end) < 48*time.Hour {
			continue
		}
		if _, err := q.DecideSRPRequest(ctx, db.DecideSRPRequestParams{
			ID: id, Status: "paid", Payout: payout, HandledBy: demoCharSera, HandlerNote: "Paid at 80 percent.",
			HandledAt: sql.NullTime{Time: end.Add(20 * time.Hour), Valid: true},
		}); err != nil {
			return err
		}
	}
	return nil
}

// writeDemoPlannedFleet files the group's next fleet, with pilots
// answering its sign-up.
func (s *demoSeed) writeDemoPlannedFleet(ctx context.Context, doctrineID, policyID int64) error {
	q := s.app.queries
	start := s.now.Add(52 * time.Hour).Truncate(15 * time.Minute)
	op, err := s.createDemoFleet(ctx, "Weekend chain sweep",
		"Full doctrine. We roll the static until something worth shooting turns up. Sign up with the hull you are bringing.",
		start, 150, demoCharSera, doctrineID, policyID)
	if err != nil {
		return err
	}
	for i, seat := range demoFleetPool[:14] {
		response := opYes
		if i%5 == 4 {
			response = opMaybe
		}
		userID := sql.NullInt64{}
		if seat.ID == demoCharacterID || seat.ID == demoCharacter2ID {
			userID = sql.NullInt64{Int64: s.userID, Valid: true}
		}
		out, _, reps := demoLogWork(seat.Ship)
		role := "DPS"
		switch {
		case reps > 0:
			role = "Logistics"
		case seat.Ship == "Sabre" || seat.Ship == "Huginn":
			role = "Tackle"
		case seat.Ship == "Claymore":
			role = "Command"
		case out == 0:
			role = "Scout"
		}
		if err := q.UpsertOpSignup(ctx, db.UpsertOpSignupParams{
			OpID: op.ID, CharacterID: seat.ID, UserID: userID, Response: response, Ship: seat.Ship, FleetRole: role,
			UpdatedAt: s.now.Add(-time.Duration(i+1) * time.Hour),
		}); err != nil {
			return err
		}
	}
	return nil
}

// writeDemoLiveFleet files the fleet that is always running, with Sable
// as its boss, a read of it, and pilots signed up who have not joined.
func (s *demoSeed) writeDemoLiveFleet(ctx context.Context, doctrineID, policyID int64, systems []int64) error {
	q := s.app.queries
	start := s.now.Add(-demoLiveFleetAge)
	op, err := s.createDemoFleet(ctx, "Wormhole roam: chain sweep",
		"Rolling the chain for targets. Nano shield doctrine, burn on the anchor, logi call for reps.",
		start, 120, demoCharacterID, doctrineID, policyID)
	if err != nil {
		return err
	}
	if err := q.AddOpCommander(ctx, db.AddOpCommanderParams{OpID: op.ID, CharacterID: demoCharSera, AddedBy: demoCharacterID, AddedAt: start}); err != nil {
		return err
	}
	for _, seat := range demoLiveSeats {
		for _, note := range seat.FleetNotes {
			if err := q.InsertOpMemberTag(ctx, db.InsertOpMemberTagParams{
				OpID: op.ID, CharacterID: seat.ID, Kind: fleetTagNote, Tag: note, SetBy: demoCharacterID, SetAt: start,
			}); err != nil {
				return err
			}
		}
	}
	for _, c := range []struct {
		id         int64
		ship, role string
	}{
		{demoPilotRosa, "Scimitar", "Logistics"},
		{demoPilotIvo, "Tengu", "DPS"},
	} {
		if err := q.UpsertOpSignup(ctx, db.UpsertOpSignupParams{
			OpID: op.ID, CharacterID: c.id, Response: opYes, Ship: c.ship, FleetRole: c.role,
			Note: "Running late, invite when you can.", UpdatedAt: start.Add(-20 * time.Minute),
		}); err != nil {
			return err
		}
	}
	members := make([]esi.FleetMember, 0, len(demoLiveSeats))
	for _, seat := range demoLiveSeats {
		members = append(members, esi.FleetMember{
			CharacterID: seat.ID, ShipTypeID: s.typeID(seat.Ship), SolarSystemID: systems[seat.System],
			WingID: seat.Wing, SquadID: seat.Squad, Role: seat.Position,
		})
	}
	tags, err := q.ListOpMemberTags(ctx, op.ID)
	if err != nil {
		return err
	}
	read := fleetRead{State: fleetReadBoss, FleetID: demoFleetMailBase, BossID: demoCharacterID, Members: members}
	comp := s.app.composeFleet(ctx, op, read, demoLiveUnits, tags)
	// The first sighting is near the start, so the fleet reads as one
	// that has been running.
	if _, err := q.UpsertFleetAttendance(ctx, s.demoSeatsParams(op.ID, demoLiveSeats, systems, start.Add(2*time.Minute))); err != nil {
		return err
	}
	if err := s.app.saveFleetRead(ctx, op, comp, s.now); err != nil {
		return err
	}
	return q.SetOpCaptureStatus(ctx, db.SetOpCaptureStatusParams{
		CaptureStatus: captureRunning, CheckedAt: sql.NullTime{Time: s.now, Valid: true}, BossID: demoCharacterID,
		Detail: "", ID: op.ID,
	})
}

// fleetExampleView is the public example of a running fleet: the demo
// group's live fleet as its fleet commander sees it, read-only.
type fleetExampleView struct {
	Live                                  *fleetLiveView
	Title, Owner, Doctrine, FormUp, About string
	Fits                                  []fleetFit
}

// handleFleetExample is GET /fleets/example/: the demo group's running
// fleet, read-only, so anyone (not only a demo visitor) can see what
// the live fleet tools show. Every pilot in it is invented, so no name
// links anywhere, and nothing on it can be changed.
func (app *Application) handleFleetExample(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	v := app.fleetViewerFor(ctx)
	op, err := app.queries.GetDemoLiveFleet(ctx, demoCommunityID)
	if err != nil || !isDemoFleet(op) || !fleetLiveOpen(op, now) {
		app.renderFleetHidden(w, r, v)
		return
	}
	row, err := app.storedComposition(ctx, op.ID)
	if err != nil || row == nil {
		app.renderFleetHidden(w, r, v)
		return
	}
	live := fleetLiveRead{row: row}
	if json.Unmarshal([]byte(row.Payload), &live.comp) != nil {
		app.renderFleetHidden(w, r, v)
		return
	}
	live.sight = live.comp.sightFor([]int64{op.FcCharacterID})
	view := app.fleetLiveFor(ctx, op, fleetViewer{}, fleetAccess{}, live, 1, now)
	view.Polling, view.TryURL, view.CanEdit, view.CanManage = false, "", false, false
	for i := range view.Structure {
		plainFleetUnit(&view.Structure[i])
	}
	fits, _ := app.fleetFits(ctx, op)
	for i := range fits {
		fits[i].Href = ""
	}
	page := &fleetExampleView{
		Live: view, Title: op.Title, Owner: app.ownerName(ctx, opOwner(op)),
		Doctrine: op.Doctrine, FormUp: op.FormUp, About: op.Description, Fits: fits,
	}
	w.Header().Set("X-Robots-Tag", "noindex")
	data := pageData{ShellData: app.PublicShell(ctx), FleetExample: page}
	app.render(ctx, w, http.StatusOK, "fleet_example.html", data)
}

// plainFleetUnit takes the links off every name in a unit.
func plainFleetUnit(u *fleetUnitView) {
	if u.Commander != nil {
		u.Commander.Plain, u.Commander.Mine = true, false
	}
	for i := range u.Members {
		u.Members[i].Plain, u.Members[i].Mine = true, false
	}
	for i := range u.Squads {
		plainFleetUnit(&u.Squads[i])
	}
}
