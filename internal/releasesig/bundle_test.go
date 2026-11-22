package releasesig

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
)

func TestBundleOpensOnlyWithATrustedSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	raw, err := NewBundle([]ed25519.PrivateKey{priv}, BundleManifest{Version: "1.2.3.4", SHA256: map[string]string{"arm64": "aa"}})
	if err != nil {
		t.Fatal(err)
	}
	m, key, err := OpenBundle([]ed25519.PublicKey{pub}, raw)
	if err != nil || m.Version != "1.2.3.4" || m.SHA256["arm64"] != "aa" || !key.Equal(pub) {
		t.Fatalf("OpenBundle = %+v, %v", m, err)
	}
	if _, _, err := OpenBundle([]ed25519.PublicKey{otherPub}, raw); err == nil {
		t.Error("a bundle signed by an untrusted key opened")
	}
	var file bundleFile
	_ = json.Unmarshal(raw, &file)
	file.Manifest, _ = json.Marshal(BundleManifest{Version: "9.9.9.9", SHA256: map[string]string{"arm64": "bb"}})
	forged, _ := json.Marshal(file)
	if _, _, err := OpenBundle([]ed25519.PublicKey{pub}, forged); err == nil {
		t.Error("a changed manifest kept its signature")
	}
}
