package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// fakeLister answers Sources with a fixed list or error.
type fakeLister struct {
	srcs []media.Source
	err  error
}

func (f fakeLister) Sources(context.Context) ([]media.Source, error) { return f.srcs, f.err }

var twoMonitors = []media.Source{
	{ID: `mon:\\.\DISPLAY1`, Kind: media.SourceScreen, Name: "DELL U2723QE", Width: 3840, Height: 2160,
		MonitorID: `mon:\\.\DISPLAY1`},
	{ID: `mon:\\.\DISPLAY2`, Kind: media.SourceScreen, Name: "DELL U2723QE", Width: 3840, Height: 2160,
		MonitorID: `mon:\\.\DISPLAY2`},
}

func TestSourcesText(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runSources(context.Background(), fakeLister{srcs: twoMonitors}, false, false, &out, &errOut); code != exitPass {
		t.Fatalf("exit %d, want %d\n%s", code, exitPass, errOut.String())
	}
	want := "mon:\\\\.\\DISPLAY1 3840x2160\nmon:\\\\.\\DISPLAY2 3840x2160\n"
	if out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
	if strings.Contains(out.String(), "DELL") {
		t.Errorf("a name was printed without -name:\n%s", out.String())
	}

	out.Reset()
	if code := runSources(context.Background(), fakeLister{srcs: twoMonitors}, true, false, &out, &errOut); code != exitPass {
		t.Fatalf("-name: exit %d", code)
	}
	if !strings.Contains(out.String(), `mon:\\.\DISPLAY2 3840x2160, name "DELL U2723QE"`) {
		t.Errorf("-name output lacks the name:\n%s", out.String())
	}
}

func TestSourcesJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runSources(context.Background(), fakeLister{srcs: twoMonitors[:1]}, false, true, &out, &errOut); code != exitPass {
		t.Fatalf("exit %d\n%s", code, errOut.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if len(got) != 1 || got[0]["id"] != `mon:\\.\DISPLAY1` || got[0]["width"] != float64(3840) ||
		got[0]["height"] != float64(2160) {
		t.Errorf("JSON %v", got)
	}
	if _, ok := got[0]["name"]; ok {
		t.Errorf("JSON has a name without -name: %v", got)
	}
}

func TestSourcesExitCodes(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runSources(context.Background(), fakeLister{}, false, false, &out, &errOut); code != exitEnv {
		t.Errorf("no monitors: exit %d, want %d (environment)", code, exitEnv)
	}
	unsupported := &media.Error{Code: media.CodeUnsupportedOS, Op: "sources", Detail: "no engine"}
	if code := runSources(context.Background(), fakeLister{err: unsupported}, false, false, &out, &errOut); code != exitEnv {
		t.Errorf("unsupported OS: exit %d, want %d", code, exitEnv)
	}
	internal := &media.Error{Code: media.CodeInternal, Op: "sources", Detail: "im_list_sources: bad JSON"}
	if code := runSources(context.Background(), fakeLister{err: internal}, false, false, &out, &errOut); code != exitFail {
		t.Errorf("internal: exit %d, want %d", code, exitFail)
	}
}

func TestSourcesRegistered(t *testing.T) {
	c, ok := commands["sources"]
	if !ok || c.run == nil {
		t.Fatal("sources is not registered")
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"sources", "-bogus"}, &out, &errOut); code != exitUsage {
		t.Errorf("a bad flag: exit %d, want %d", code, exitUsage)
	}
	if code := run(context.Background(), []string{"sources", "extra"}, &out, &errOut); code != exitUsage {
		t.Errorf("an argument: exit %d, want %d", code, exitUsage)
	}
}
