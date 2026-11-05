package bridge

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/media/fake"
)

// The share sheet renders under every scenario of 05's fake (ISSHONI_FAKE_AUDIO, 05 §13.3; README WA77): the SPA's
// snapshot test (web/src/share/native/scenarios.test.tsx) renders each fixture of scenarioDir, and this test keeps
// those fixtures equal to what Audio.FriendsHear() answers with the real 05 service on the fake engine, 6 s after
// the share sheet started watching (normal's browser mic prompt has come, fast-path's Discord hasn't). Run it with
// -update-scenarios to write them again.
var updateScenarios = flag.Bool("update-scenarios", false, "write web/src/share/native/testdata/friendsHear/*.json")

// scenarioDir holds one FriendsHear JSON per scenario, named after it.
var scenarioDir = filepath.Join("..", "..", "..", "web", "src", "share", "native", "testdata", "friendsHear")

// fakeAudioScenarios are 05 §13.3's scenarios (internal/media/fake's audioScenarios).
var fakeAudioScenarios = []string{"normal", "degraded", "degrade-live", "old-windows", "no-device", "fast-path",
	"shared-mic", "many"}

// scenarioWatch is how long the sheet watches before the fixture is taken.
const scenarioWatch = 6 * time.Second

// scenarioStore is 05's settings store in memory, at the defaults.
type scenarioStore struct {
	mu sync.Mutex
	s  audio.Settings
}

func (m *scenarioStore) AudioSettings() audio.Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
}

func (m *scenarioStore) SaveAudioSettings(s audio.Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s = s
	return nil
}

// scenarioFriendsHear is Audio.FriendsHear() of the bound service over 05's service on the fake's scenario, after
// the sheet watched for scenarioWatch, as JSON.
func scenarioFriendsHear(t *testing.T, name string) []byte {
	t.Helper()
	var out []byte
	synctest.Test(t, func(t *testing.T) {
		eng := fake.NewEngine(fake.EngineConfig{AudioScenario: name})
		defer func() { _ = eng.Close() }()
		svc := audio.New(eng, &scenarioStore{s: audio.DefaultSettings()},
			audio.SelfInfo{PID: 4242, WebViewDataDir: `C:\isshoni-test\WebView2`}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = svc.Run(ctx)
		}()
		a := &Audio{core: &fakeCore{}, policy: svc}
		if err := a.Watch(true); err != nil {
			t.Fatalf("%s: Watch: %v", name, err)
		}
		time.Sleep(scenarioWatch)
		synctest.Wait()
		fh, err := a.FriendsHear()
		if err != nil {
			t.Fatalf("%s: FriendsHear: %v", name, err)
		}
		_ = a.Watch(false)
		cancel()
		<-done
		_ = svc.Close()
		out, err = json.MarshalIndent(fh, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
	})
	return append(out, '\n')
}

// TestFakeScenarioFixtures: the SPA's fixtures are what the bridge answers under each scenario; there is one per
// scenario and no other.
func TestFakeScenarioFixtures(t *testing.T) {
	for _, name := range fakeAudioScenarios {
		got := scenarioFriendsHear(t, name)
		path := filepath.Join(scenarioDir, name+".json")
		if *updateScenarios {
			if err := os.MkdirAll(scenarioDir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // G306: a fixture in the repository
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path) //nolint:gosec // G304: the fixture of a known scenario
		if err != nil {
			t.Fatalf("%s: %v (run go test -run TestFakeScenarioFixtures -update-scenarios)", name, err)
		}
		// Compared as JSON values: a Windows checkout may have changed the file's line ends.
		var g, w any
		if err := json.Unmarshal(got, &g); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want, &w); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s: Audio.FriendsHear() differs from %s; run go test -run TestFakeScenarioFixtures "+
				"-update-scenarios and check the share sheet's snapshots\n got %s", name, path, got)
		}
	}
	entries, err := os.ReadDir(scenarioDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !slices.Contains(fakeAudioScenarios, name) {
			t.Errorf("%s isn't a fixture of a scenario", e.Name())
		}
	}
}
