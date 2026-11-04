package app

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestBattleFightOwnCharactersOnly pins who a posted fight can be
// filed for: a paired device stores a fight for one of its own
// account's characters, refuses one for a character on another
// account, and the stored report opens for its owner only.
func TestBattleFightOwnCharactersOnly(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, 90000201, "Watcher Pilot")
	other, _ := q.CreateUser(ctx)
	seedCharacter(t, q, other.ID, 90000301, "Someone Else")
	cookie := sessionCookie(t, app, u.ID, 90000201, "Watcher Pilot")
	otherCookie := sessionCookie(t, app, other.ID, 90000301, "Someone Else")
	token, priv := pairDevice(t, app, cookie, "Fight PC")

	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	post := func(listener string, charID int64, ts time.Time) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"listener": listener, "character_id": charID, "session": "20261104_120000_1.txt",
			"start": at, "end": at.Add(2 * time.Second),
			"series": map[string][]int64{
				"damage_out": {100, 0, 50}, "damage_in": {0, 200, 0},
				"reps_out": {0, 0, 0}, "reps_in": {0, 0, 0}, "cap_in": {0, 0, 0},
			},
			"parties": []map[string]any{{"name": "Bad Guy", "ticker": "EVIL", "hull": "Vexor", "pilot": true, "damage_out": 150, "damage_in": 200, "first": at, "last": at.Add(2 * time.Second)}},
			"slices":  []map[string]any{{"at": 0, "rows": [][6]int64{{0, 150, 200, 0, 0, 0}}}},
			"lines":   4,
		})
		stamp := strconv.FormatInt(ts.Unix(), 10)
		sig := ed25519.Sign(priv, []byte(axonSignatureText(http.MethodPost, "/api/intel/v1/fights", stamp, body)))
		req := httptest.NewRequest(http.MethodPost, "/api/intel/v1/fights", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Axon-Timestamp", stamp)
		req.Header.Set("X-Axon-Signature", base64.StdEncoding.EncodeToString(sig))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := post("Someone Else", 90000301, time.Now()); rec.Code != http.StatusForbidden {
		t.Fatalf("a fight for another account's character: %d %s, want 403", rec.Code, rec.Body.String())
	}
	rec := post("Watcher Pilot", 90000201, time.Now().Add(time.Second))
	var res struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &res) != nil || res.ID == 0 || res.Status != "stored" {
		t.Fatalf("a fight for the account's own character: %d %s", rec.Code, rec.Body.String())
	}
	path := "/intel/battle/" + strconv.FormatInt(res.ID, 10) + "/"
	if code, body := getPage(t, app, cookie, path); code != http.StatusOK || !strings.Contains(body, "Bad Guy") {
		t.Errorf("the owner's report: %d", code)
	}
	if code, _ := getPage(t, app, otherCookie, path); code != http.StatusNotFound {
		t.Errorf("another account opening the report: %d, want 404", code)
	}
}
