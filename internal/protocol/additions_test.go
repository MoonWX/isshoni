package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// m2Fixtures are the fixtures of M2's additions (M2 01 §10.3, §10.4, §14): share.update with paused (set and
// cleared), room.state with a paused share, and user.connections with a device name. The M1 fixtures next to them
// (share.update.json, room.state.json, user.connections.json) stay as they are: they are what M1 clients send and
// read.
var m2Fixtures = strings.Fields(`share.update.paused share.update.resume room.state.paused user.connections.device`)

func TestM2FixtureSet(t *testing.T) {
	for _, name := range m2Fixtures {
		if _, err := os.Stat(filepath.Join(fixtureDir, name+".json")); err != nil {
			t.Errorf("fixture %s.json of M2 01 §14: %v", name, err)
		}
	}
}

// loadFixture decodes one fixture of testdata/v1 into T along the production path (ParseEnvelope, Decode) and
// encodes it again with Marshal.
func loadFixture[T any](t *testing.T, name string) (T, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	env, err := ParseEnvelope(raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	v, err := Decode[T](env)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out, err := Marshal(env.Type, env.ID, env.Re, v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v, out
}

// TestSharePauseIsAdditive: share.update's paused and ShareStatusPaused (M2 01 §10.3, feature share.pause). An M1
// share.update has no paused and encodes again without the key; paused true and false are both kept, so a resume
// (false) is not mistaken for "leave it as it is" (absent). room.state carries a paused share with its layers and
// codec, and the M1 room.state holds no paused share.
func TestSharePauseIsAdditive(t *testing.T) {
	m1, out := loadFixture[ShareUpdate](t, "share.update")
	if m1.Paused != nil {
		t.Errorf("share.update.json (M1): paused = %v, want absent", *m1.Paused)
	}
	if strings.Contains(string(out), `"paused"`) {
		t.Errorf("an M1 share.update encodes with a paused key: %s", out)
	}
	for name, want := range map[string]bool{"share.update.paused": true, "share.update.resume": false} {
		u, out := loadFixture[ShareUpdate](t, name)
		if u.Paused == nil || *u.Paused != want {
			t.Errorf("%s.json: paused = %v, want %v", name, u.Paused, want)
		}
		if u.Label != nil || u.Preset != "" {
			t.Errorf("%s.json: label %v, preset %q; want neither", name, u.Label, u.Preset)
		}
		if key := `"paused":` + map[bool]string{true: "true", false: "false"}[want]; !strings.Contains(string(out), key) {
			t.Errorf("%s.json encodes without %s: %s", name, key, out)
		}
	}

	if !ShareStatusPaused.Valid() || ShareStatusPaused != "paused" {
		t.Errorf("ShareStatusPaused = %q, Valid %v", ShareStatusPaused, ShareStatusPaused.Valid())
	}
	st, _ := loadFixture[RoomState](t, "room.state.paused")
	if len(st.Shares) != 1 {
		t.Fatalf("room.state.paused.json: %d shares, want 1", len(st.Shares))
	}
	s := st.Shares[0]
	if s.Status != ShareStatusPaused || !reflect.DeepEqual(s.Layers, []VideoLayer{VideoLayerHigh, VideoLayerLow}) ||
		s.Codec != CodecH264High {
		t.Errorf("room.state.paused.json: status %q, layers %v, codec %q; want paused with both layers and its codec",
			s.Status, s.Layers, s.Codec)
	}
	m1State, _ := loadFixture[RoomState](t, "room.state")
	for _, s := range m1State.Shares {
		if s.Status == ShareStatusPaused {
			t.Errorf("room.state.json (M1) holds a paused share %s", s.ID)
		}
	}
}

// TestOwnConnectionDeviceNameIsAdditive: OwnConnection.DeviceName (M2 01 §10.4). user.connections.json, which has
// none, decodes to "" everywhere and encodes again without the key; user.connections.device.json names the desktop
// app's device only (a cookie connection has no device).
func TestOwnConnectionDeviceNameIsAdditive(t *testing.T) {
	before, out := loadFixture[UserConnections](t, "user.connections")
	for _, c := range before.Connections {
		if c.DeviceName != "" {
			t.Errorf("user.connections.json: %s has device name %q, want none", c.ID, c.DeviceName)
		}
	}
	if strings.Contains(string(out), `"deviceName"`) {
		t.Errorf("a connection without a device name encodes with the key: %s", out)
	}

	with, out := loadFixture[UserConnections](t, "user.connections.device")
	got := map[string]string{}
	for _, c := range with.Connections {
		got[string(c.Kind)] = c.DeviceName
	}
	if want := map[string]string{"web": "", "desktop": "Gaming PC"}; !reflect.DeepEqual(got, want) {
		t.Errorf("user.connections.device.json: device names by kind %v, want %v", got, want)
	}
	if n := strings.Count(string(out), `"deviceName"`); n != 1 {
		t.Errorf("user.connections.device.json encodes %d deviceName keys, want 1: %s", n, out)
	}
	// The embedded ConnectionInfo stays inlined next to the new field.
	var flat []map[string]any
	if err := json.Unmarshal(mustJSON(t, with.Connections), &flat); err != nil {
		t.Fatal(err)
	}
	if flat[1]["id"] != "c_k3v9q2m7xw4pa8d1" || flat[1]["deviceName"] != "Gaming PC" || flat[1]["ConnectionInfo"] != nil {
		t.Errorf("OwnConnection encodes as %v, want ConnectionInfo's fields inlined beside deviceName", flat[1])
	}
}

// TestM2FeaturesKnown: the two features M2 servers serve are known features, so the hub can list them in welcome and
// a client's hello may ask for them (01 §6.2); user.connections is the one message behind its feature, and
// share.pause gates no message (only share.update's paused and the paused status).
func TestM2FeaturesKnown(t *testing.T) {
	for _, f := range []Feature{FeatureSharePause, FeatureUserConnections} {
		if !f.Valid() {
			t.Errorf("%s is not a known feature", f)
		}
	}
	for _, s := range Registry {
		switch {
		case s.Type == MessageTypeUserConnections && s.Feature != FeatureUserConnections:
			t.Errorf("user.connections is behind %q, want %q", s.Feature, FeatureUserConnections)
		case s.Feature == FeatureSharePause:
			t.Errorf("%s is behind share.pause, which gates no message type", s.Type)
		case s.Type == MessageTypeShareUpdate && s.Feature != "":
			t.Errorf("share.update is behind %q: it is baseline, only its paused field needs share.pause", s.Feature)
		}
	}
}
