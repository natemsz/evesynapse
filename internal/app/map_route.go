package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// The map's routes are planned in the browser (map.js), from the
// universe payload's gates and the live threat layer; the server's
// one part is handing a planned route to the game client's
// autopilot, through ESI's waypoint endpoint, as one of the
// reader's characters holding esi-ui.write_waypoint.v1.

const waypointScope = "esi-ui.write_waypoint.v1"

// maxWaypoints caps a sent route: the game keeps a long route
// fine, but every waypoint is its own ESI call.
const maxWaypoints = 100

// mapRouteRequest is the map's POST /map/route body. Mode "dest"
// sets Systems[0] as the autopilot destination; mode "route" sets
// every system in order as a waypoint, replacing the old route.
type mapRouteRequest struct {
	Mode    string  `json:"mode"`
	Systems []int64 `json:"systems"`
}

// mapRouteResponse is its answer, shown in the route panel.
type mapRouteResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	// Sent counts the waypoints the game accepted before any
	// failure.
	Sent int `json:"sent"`
}

// isKSpaceSystem: the gate-connected systems a route can hold.
func isKSpaceSystem(id int64) bool { return id >= 30000000 && id < 31000000 }

// waypointPaths turns a route request into its ESI calls, in order:
// the first clears the old route, the rest append.
func waypointPaths(req mapRouteRequest) ([]string, error) {
	systems := req.Systems
	switch req.Mode {
	case "dest":
		if len(systems) == 0 {
			return nil, errors.New("no destination")
		}
		systems = systems[:1]
	case "route":
		if len(systems) == 0 {
			return nil, errors.New("an empty route")
		}
		if len(systems) > maxWaypoints {
			return nil, fmt.Errorf("a route of more than %d jumps", maxWaypoints)
		}
	default:
		return nil, errors.New("an unknown mode")
	}
	paths := make([]string, 0, len(systems))
	for i, id := range systems {
		if !isKSpaceSystem(id) {
			return nil, fmt.Errorf("system %d", id)
		}
		paths = append(paths, fmt.Sprintf("/ui/autopilot/waypoint/?add_to_beginning=false&clear_other_waypoints=%t&destination_id=%d", i == 0, id))
	}
	return paths, nil
}

// waypointCharacter picks the character to steer: the session
// character when it holds the scope, else the first that does.
func waypointCharacter(chars []db.Character, sessionID int64) *db.Character {
	var chosen *db.Character
	for i := range chars {
		if !module.CharacterHasScope(chars[i], waypointScope) {
			continue
		}
		if chars[i].CharacterID == sessionID {
			return &chars[i]
		}
		if chosen == nil {
			chosen = &chars[i]
		}
	}
	return chosen
}

func writeRouteJSON(w http.ResponseWriter, status int, res mapRouteResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// handleMapRoute is POST /map/route: the map's "Set destination"
// and "Send route" buttons.
func (app *Application) handleMapRoute(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		writeRouteJSON(w, http.StatusUnauthorized, mapRouteResponse{Message: "Sign in to send routes to your game client."})
		return
	}
	var req mapRouteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeRouteJSON(w, http.StatusBadRequest, mapRouteResponse{Message: "That route could not be read."})
		return
	}
	paths, err := waypointPaths(req)
	if err != nil {
		writeRouteJSON(w, http.StatusBadRequest, mapRouteResponse{Message: "The game cannot take " + err.Error() + "."})
		return
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("waypoint: characters for user %d: %v", userID, err)
		writeRouteJSON(w, http.StatusInternalServerError, mapRouteResponse{Message: "That did not go through — please try again."})
		return
	}
	ch := waypointCharacter(chars, app.SessionCharacterID(ctx))
	if ch == nil {
		writeRouteJSON(w, http.StatusForbidden, mapRouteResponse{Message: "None of your characters can set routes yet — sign in again and allow EveSynapse to set your autopilot waypoints."})
		return
	}
	token, err := app.AccessToken(ctx, *ch)
	if err != nil {
		logging.Errorf("waypoint: token for character %d: %v", ch.CharacterID, err)
		writeRouteJSON(w, http.StatusBadGateway, mapRouteResponse{Message: "That did not go through — please try again."})
		return
	}
	for i, path := range paths {
		if err := app.esi.PostAuthedNoContent(ctx, token, path, nil); err != nil {
			res := mapRouteResponse{Sent: i, Message: "The game did not answer — the route was not set."}
			if i > 0 {
				res.Message = fmt.Sprintf("The game stopped answering after %d of %d waypoints.", i, len(paths))
			}
			var se *esi.StatusError
			if errors.As(err, &se) && se.Code == http.StatusForbidden {
				res.Message = ch.Name + " has not allowed EveSynapse to set waypoints — sign in again to grant it."
			} else {
				logging.Errorf("waypoint: %s as %d: %v", path, ch.CharacterID, err)
			}
			writeRouteJSON(w, http.StatusBadGateway, res)
			return
		}
	}
	msg := "Destination set in " + ch.Name + "'s autopilot."
	if req.Mode == "route" {
		msg = fmt.Sprintf("Route of %d jumps sent to %s's autopilot.", len(paths), ch.Name)
	}
	writeRouteJSON(w, http.StatusOK, mapRouteResponse{OK: true, Sent: len(paths), Message: msg})
}

// mapRouteView is what the map page knows of the reader for routing:
// whether they may send routes, and where their character last was.
type mapRouteView struct {
	SignedIn  bool
	Waypoints bool
	// Here is the session character's last stored system (0 when
	// unknown).
	Here int64
}

func (app *Application) loadMapRouteView(ctx context.Context) mapRouteView {
	var v mapRouteView
	chars := app.sessionCharacters(ctx)
	if len(chars) == 0 {
		return v
	}
	v.SignedIn = true
	v.Waypoints = waypointCharacter(chars, 0) != nil
	if id := app.SessionCharacterID(ctx); id != 0 {
		var loc esi.Location
		if app.loadCorpSnapshot(ctx, id, esi.SnapLocation, &loc) && isKSpaceSystem(loc.SolarSystemID) {
			v.Here = loc.SolarSystemID
		}
	}
	return v
}
