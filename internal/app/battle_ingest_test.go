package app

import (
	"bytes"
	"compress/gzip"
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

// TestBattleFightOwnCharactersOnly pins who a game log's fights can
// be filed for: a paired device's log is queued and stored for one of
// its own account's characters, refused for a character on another
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

	at := time.Now().UTC().Add(-time.Hour)
	stamp := func(d time.Duration) string { return at.Add(d).Format("2006.01.02 15:04:05") }
	post := func(listener string, charID int64, ts time.Time) *httptest.ResponseRecorder {
		text := strings.Join([]string{
			"  Gamelog",
			"  Listener: " + listener,
			"[ " + stamp(0) + " ] (combat) <color=0xff00ffff><b>100</b> <color=0x77ffffff><font size=10>to</font> <b><color=0xffffffff>Bad Guy</b>[EVIL](Vexor)<font size=10><color=0x77ffffff> - Heavy Pulse Laser II - Hits",
			"[ " + stamp(time.Second) + " ] (combat) <color=0xffcc0000><b>200</b> <color=0x77ffffff><font size=10>from</font> <b><color=0xffffffff>Bad Guy</b>[EVIL](Vexor)<font size=10><color=0x77ffffff> - Hammerhead II - Smashes",
			"",
		}, "\r\n")
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write([]byte(text))
		_ = zw.Close()
		body := gz.Bytes()
		path := "/api/intel/v1/gamelog?session=20261104_120000_" + strconv.FormatInt(charID, 10) + ".txt"
		sts := strconv.FormatInt(ts.Unix(), 10)
		sig := ed25519.Sign(priv, []byte(axonSignatureText(http.MethodPost, path, sts, body)))
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Axon-Timestamp", sts)
		req.Header.Set("X-Axon-Signature", base64.StdEncoding.EncodeToString(sig))
		req.Header.Set("Content-Type", "application/gzip")
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := post("Someone Else", 90000301, time.Now()); rec.Code != http.StatusForbidden {
		t.Fatalf("a log for another account's character: %d %s, want 403", rec.Code, rec.Body.String())
	}
	rec := post("Watcher Pilot", 90000201, time.Now().Add(time.Second))
	var res struct {
		Queued bool `json:"queued"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &res) != nil || !res.Queued {
		t.Fatalf("a log for the account's own character: %d %s", rec.Code, rec.Body.String())
	}
	app.battleQueue().drain(ctx, app.readBattleJob)
	fights, err := q.ListBattleFightsByUser(ctx, u.ID)
	if err != nil || len(fights) != 1 {
		t.Fatalf("stored fights: %d, %v", len(fights), err)
	}
	path := "/intel/battle/" + strconv.FormatInt(fights[0].ID, 10) + "/"
	if code, body := getPage(t, app, cookie, path); code != http.StatusOK || !strings.Contains(body, "Bad Guy") {
		t.Errorf("the owner's report: %d", code)
	}
	if code, _ := getPage(t, app, otherCookie, path); code != http.StatusNotFound {
		t.Errorf("another account opening the report: %d, want 404", code)
	}
}
