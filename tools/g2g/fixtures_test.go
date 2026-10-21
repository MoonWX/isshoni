package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image/jpeg"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The decoder's recorded fixtures: screenshots of the real clock page (frozen with ?freeze=<ms>), rendered by
// headless Chrome, with a viewer window showing an earlier screenshot of the same screen after a JPEG round trip,
// scaled by Chrome (what the video codec and the viewer do to it). This re-records them, with G2G_CHROME naming a
// Chrome or Chromium binary:
//
//	G2G_CHROME=/path/to/chrome go -C tools test ./g2g -run TestRecordFixtures -args -record
//
// testdata/fixtures.json says what each one holds.
var record = flag.Bool("record", false, "re-record testdata/*.png and fixtures.json with Chrome (G2G_CHROME)")

type fixture struct {
	File   string   `json:"file"`
	What   string   `json:"what"`
	Clock  uint16   `json:"clock"`            // the clock's own strip
	Copy   uint16   `json:"copy"`             // the largest copy; for a transition, the older of its two frames
	CopyTo *uint16  `json:"copyTo,omitempty"` // a transition: the newer frame
	Scale  float64  `json:"scale"`            // the largest copy's scale
	Nested []uint16 `json:"nested,omitempty"` // the smaller copies at scale ≥ 0.25, largest first
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx []fixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx) == 0 {
		t.Fatal("testdata/fixtures.json lists no fixture")
	}
	return fx
}

// TestFixtures decodes the recorded screenshots: the clock at its known position, the largest copy at its scale,
// the nested copies, and a copy caught between two frames within half a frame of them.
func TestFixtures(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(strings.TrimSuffix(fx.File, ".png"), func(t *testing.T) {
			f := readPNG(t, filepath.Join("testdata", fx.File))
			all := find(f, f.bounds())
			if len(all) == 0 {
				t.Fatal("found no strip")
			}
			clock := all[0]
			if math.Abs(clock.scale()-1) > clockScaleTo || dist(clock.strip, clockStrip()) > 1 || clock.Value != fx.Clock {
				t.Fatalf("clock: found %+v (scale %.3f), want %d at (24, 24)", clock, clock.scale(), fx.Clock)
			}
			var copies []found
			for _, c := range all[1:] {
				if c.scale() >= minCopyScale-0.01 {
					copies = append(copies, c)
				}
			}
			if len(copies) != 1+len(fx.Nested) {
				t.Fatalf("found %d copies at scale ≥ 0.25 (%+v), want %d", len(copies), copies, 1+len(fx.Nested))
			}
			c := copies[0]
			if math.Abs(c.scale()-fx.Scale) > 0.005 {
				t.Errorf("copy: scale %.4f, want %.3f", c.scale(), fx.Scale)
			}
			if fx.CopyTo == nil {
				if c.Value != fx.Copy || c.Ambiguous != 0 {
					t.Errorf("copy: read %d (%d ambiguous cells), want %d", c.Value, c.Ambiguous, fx.Copy)
				}
			} else {
				step := wrapDiff(int(*fx.CopyTo) - int(fx.Copy))
				off := wrapDiff(int(c.Value) - int(fx.Copy))
				if c.Ambiguous == 0 || off < -step/2 || off > step+step/2 {
					t.Errorf("transition copy: read %d (%d ambiguous cells), want within half a frame of %d–%d",
						c.Value, c.Ambiguous, fx.Copy, *fx.CopyTo)
				}
			}
			for i, v := range fx.Nested {
				if copies[1+i].Value != v {
					t.Errorf("nested copy %d: read %d at scale %.3f, want %d", i, copies[1+i].Value, copies[1+i].scale(), v)
				}
			}
		})
	}
}

// staticCapturer returns the same screenshot every time.
type staticCapturer struct{ f *Frame }

func (s staticCapturer) grab() (*Frame, error) { return s.f, nil }
func (staticCapturer) refreshHz() int          { return 60 }
func (staticCapturer) close() error            { return nil }

// TestCalibrateFixtures: calibration picks the clock and the largest copy in each recorded screenshot.
func TestCalibrateFixtures(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(strings.TrimSuffix(fx.File, ".png"), func(t *testing.T) {
			m := newMeter(staticCapturer{readPNG(t, filepath.Join("testdata", fx.File))}, t.Logf, nil)
			m.interval = time.Millisecond
			if err := m.calibrate(context.Background(), 20*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			if dist(m.clock, clockStrip()) > 1 || math.Abs(m.copy.scale()-fx.Scale) > 0.005 {
				t.Errorf("calibrated clock %+v and copy %+v (scale %.3f), want the copy at scale %.3f", m.clock, m.copy,
					m.copy.scale(), fx.Scale)
			}
		})
	}
}

// The recorded scenes. Times are performance.now() values in ms: v is the time mod 65536, and the background is
// the text page for an even 5 s period and the noise for an odd one.
var scenes = []struct {
	file, what string
	clockT     int     // the screen's time
	copyT      []int   // the viewer shows the screen at copyT[0], which shows it at copyT[1] … (nested)
	blendT     int     // > 0: the copy is a blend of copyT[0] and blendT (a transition)
	scale      float64 // the viewer's video size, as a scale of the screen (whole pixels at 1280 × 720)
}{
	{"text-half.png", "the text page, a copy at 50 %", 62480, []int{62343}, 0, 0.5},
	{"noise-nested.png", "the noise, a copy at 50 % with a nested copy at 25 %", 66200, []int{66020, 65840}, 0, 0.5},
	{"cut-transition.png", "the noise, a copy of the text page at 42.5 % caught between two frames", 75050,
		[]int{74890}, 74907, 0.425},
}

const recW, recH = 1280, 720

// TestRecordFixtures re-records the fixtures (with -record).
func TestRecordFixtures(t *testing.T) {
	if !*record {
		t.Skip("re-records the fixtures with -record")
	}
	chrome := os.Getenv("G2G_CHROME")
	if chrome == "" {
		t.Fatal("G2G_CHROME names the Chrome binary to record with")
	}
	var mu sync.Mutex
	images := map[string][]byte{}
	mux := http.NewServeMux()
	mux.Handle("/", clockHandler())
	mux.HandleFunc("GET /img/{name}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b, ok := images[r.PathValue("name")]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /viewer", viewerPage)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	n := 0
	shot := func(path string) *Frame {
		n++
		out := filepath.Join(t.TempDir(), fmt.Sprintf("shot%d.png", n))
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		args := []string{
			"--headless=new", "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=1",
			fmt.Sprintf("--window-size=%d,%d", recW, recH), "--user-data-dir=" + t.TempDir(), "--no-first-run",
			"--no-default-browser-check", "--virtual-time-budget=3000", "--screenshot=" + out, srv.URL + path,
		}
		cmd := exec.CommandContext(ctx, chrome, args...) //nolint:gosec // G204: the Chrome that G2G_CHROME names
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("chrome: %v\n%s", err, b)
		}
		f := readPNG(t, out)
		if f.W != recW || f.H != recH {
			t.Fatalf("chrome's screenshot is %d × %d, want %d × %d", f.W, f.H, recW, recH)
		}
		return f
	}
	publish := func(name string, f *Frame) {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, f.image(), &jpeg.Options{Quality: 70}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		images[name] = buf.Bytes()
		mu.Unlock()
	}
	// screenAt is the screen at time ts: the clock page, with the viewer showing the screen at the next time in
	// the chain, recursively; the innermost screen has no viewer.
	var screenAt func(chain []int, blend int, scale float64, id string) *Frame
	screenAt = func(chain []int, blend int, scale float64, id string) *Frame {
		ts := chain[0]
		if len(chain) == 1 {
			return shot("/?freeze=" + strconv.Itoa(ts))
		}
		inner := screenAt(chain[1:], 0, scale, id+"i")
		if blend > 0 {
			inner = blendFrames(inner, shot("/?freeze="+strconv.Itoa(blend)), 0.5)
		}
		publish(id, inner)
		return shot(fmt.Sprintf("/viewer?t=%d&img=%s&scale=%g", ts, id, scale))
	}

	var fx []fixture
	for i, sc := range scenes {
		chain := append([]int{sc.clockT}, sc.copyT...)
		f := screenAt(chain, sc.blendT, sc.scale, fmt.Sprintf("s%d", i))
		writePNG(t, filepath.Join("testdata", sc.file), f)
		e := fixture{File: sc.file, What: sc.what, Clock: v16(sc.clockT), Copy: v16(sc.copyT[0]), Scale: sc.scale}
		if sc.blendT > 0 {
			to := v16(sc.blendT)
			e.CopyTo = &to
		}
		s := sc.scale * sc.scale
		for _, ts := range sc.copyT[1:] {
			if s >= minCopyScale {
				e.Nested = append(e.Nested, v16(ts))
			}
			s *= sc.scale
		}
		fx = append(fx, e)
	}
	b, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "fixtures.json"), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func v16(ms int) uint16 { return uint16(ms & 0xffff) } //nolint:gosec // G115: masked to 16 bits

// viewerPage is the screen with a viewer: the clock page frozen at t, and a window at the bottom-right whose video
// shows the image img scaled to scale of the screen, as Chrome scales a video.
func viewerPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ts, err1 := strconv.Atoi(q.Get("t"))
	scale, err2 := strconv.ParseFloat(q.Get("scale"), 64)
	img := q.Get("img")
	if err1 != nil || err2 != nil || strings.ContainsAny(img, `"<>&/`) {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	vw, vh := int(math.Round(recW*scale)), int(math.Round(recH*scale))
	const pad, bar = 8, 32
	ww, wh := vw+2*pad, vh+bar+2*pad
	x, y := recW-ww-8, recH-wh-8
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	//nolint:gosec // G705: t and scale are numbers and img has no markup characters (checked above)
	fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><style>
html, body { margin: 0; height: 100%%; overflow: hidden; }
iframe { position: fixed; left: 0; top: 0; width: 100%%; height: 100%%; border: 0; }
.win { position: fixed; left: %dpx; top: %dpx; width: %dpx; height: %dpx; background: #202124;
  box-shadow: 0 0 14px rgba(0, 0, 0, 0.6); border-radius: 8px; overflow: hidden; }
.bar { height: %dpx; background: #35363a; }
.video { position: absolute; left: %dpx; top: %dpx; width: %dpx; height: %dpx; background: #000; }
.video img { display: block; width: 100%%; height: 100%%; }
</style></head><body>
<iframe src="/?freeze=%d"></iframe>
<div class="win"><div class="bar"></div><div class="video"><img src="/img/%s" alt=""></div></div>
</body></html>
`, x, y, ww, wh, bar, pad, bar+pad, vw, vh, ts, img)
}
