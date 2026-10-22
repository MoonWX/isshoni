package app

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgsOpenURLRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want Args
	}{
		{
			name: "the registered command",
			args: []string{"--open-url", "isshoni://watch.example.com/r/lounge"},
			want: Args{OpenURL: true, Link: "isshoni://watch.example.com/r/lounge"},
		},
		{
			name: "no other flag is honored, one-shot modes included",
			args: []string{"--open-url", "isshoni://watch.example.com/r/lounge", "--purge-data", "--server",
				"https://evil.example", "--log-level", "debug", "--autostart", "--version"},
			want: Args{OpenURL: true, Link: "isshoni://watch.example.com/r/lounge", Ignored: []string{"--purge-data",
				"--server", "(positional)", "--log-level", "(positional)", "--autostart", "--version"}},
		},
		{
			name: "flags before --open-url are ignored too",
			args: []string{"--forget-credentials", "--open-url", "ISSHONI://watch.example.com/"},
			want: Args{OpenURL: true, Link: "ISSHONI://watch.example.com/", Ignored: []string{"--forget-credentials"}},
		},
		{
			name: "--relaunched first stays",
			args: []string{"--relaunched", "--open-url", "isshoni://watch.example.com/"},
			want: Args{OpenURL: true, Relaunched: true, Link: "isshoni://watch.example.com/"},
		},
		{
			name: "--relaunched elsewhere doesn't",
			args: []string{"--open-url", "isshoni://watch.example.com/", "--relaunched"},
			want: Args{OpenURL: true, Link: "isshoni://watch.example.com/", Ignored: []string{"--relaunched"}},
		},
		{
			name: "a value that isn't an isshoni: link",
			args: []string{"--open-url", "https://watch.example.com/r/lounge"},
			want: Args{OpenURL: true, Ignored: []string{"--open-url (not an isshoni: link)"}},
		},
		{
			name: "a smuggled quote makes a malformed link, not a flag",
			args: []string{"--open-url", `isshoni://x/" --purge-data "`},
			want: Args{OpenURL: true, Link: `isshoni://x/" --purge-data "`},
		},
		{
			name: "the value is taken even when it looks like a flag",
			args: []string{"--open-url", "--purge-data"},
			want: Args{OpenURL: true, Ignored: []string{"--open-url (not an isshoni: link)"}},
		},
		{
			name: "no value",
			args: []string{"--version", "--open-url"},
			want: Args{OpenURL: true, Ignored: []string{"--version"}},
		},
		{
			name: "the = form",
			args: []string{"--open-url=isshoni://watch.example.com/"},
			want: Args{OpenURL: true, Link: "isshoni://watch.example.com/"},
		},
		{
			name: "only the first --open-url counts",
			args: []string{"--open-url", "isshoni://a.example.com/", "--open-url", "isshoni://b.example.com/"},
			want: Args{OpenURL: true, Link: "isshoni://a.example.com/", Ignored: []string{"--open-url", "(positional)"}},
		},
		{
			name: "a scheme that only looks like isshoni",
			args: []string{"--open-url", "isshoni\u017f://x/"},
			want: Args{OpenURL: true, Ignored: []string{"--open-url (not an isshoni: link)"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseArgs(tc.args); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseArgs(%q)\n got %+v\nwant %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want Args
	}{
		{nil, Args{}},
		{[]string{"--version"}, Args{OneShot: ModeVersion}},
		{[]string{"--self-check=C:\\tmp\\check.json"}, Args{OneShot: ModeSelfCheck, SelfCheckPath: `C:\tmp\check.json`}},
		{[]string{"--forget-credentials"}, Args{OneShot: ModeForgetCredentials}},
		{[]string{"--purge-data", "--version"}, Args{OneShot: ModePurgeData, Ignored: []string{"--version"}}},
		{[]string{"--update-now"}, Args{OneShot: ModeUpdateNow}},
		{[]string{"-Embedding"}, Args{OneShot: ModeEmbedding}},
		{[]string{"-embedding"}, Args{OneShot: ModeEmbedding}},
		{[]string{"--sound-check-helper"}, Args{SoundCheckHelper: true}},
		{[]string{"--autostart", "--sound-check-helper"}, Args{Autostart: true, Ignored: []string{"--sound-check-helper"}}},
		{[]string{"--server", "watch.example.com"}, Args{Server: "watch.example.com"}},
		{[]string{"--server=http://localhost:5173"}, Args{Server: "http://localhost:5173"}},
		{[]string{"--server"}, Args{Ignored: []string{"--server"}}},
		{[]string{"--server", "--autostart"}, Args{Autostart: true, Ignored: []string{"--server"}}},
		{[]string{"--log-level", "debug"}, Args{Debug: true}},
		{[]string{"--log-level=debug", "--autostart"}, Args{Debug: true, Autostart: true}},
		{[]string{"--log-level", "info"}, Args{}},
		{[]string{"--log-level", "trace"}, Args{Ignored: []string{"--log-level"}}},
		{[]string{"--updated-from=0.2.0"}, Args{UpdatedFrom: "0.2.0"}},
		{[]string{"--updated-from", "0.2.0-rc.1"}, Args{UpdatedFrom: "0.2.0-rc.1"}},
		{[]string{"--updated-from=../../x"}, Args{Ignored: []string{"--updated-from"}}},
		{[]string{"--relaunched", "--autostart"}, Args{Relaunched: true, Autostart: true}},
		{[]string{"isshoni://watch.example.com/r/secret-room"}, Args{Ignored: []string{"(positional)"}}},
		{[]string{"--unknown-flag=value", "--x"}, Args{Ignored: []string{"--unknown-flag", "--x"}}},
		{[]string{"--isshoni://watch.example.com/r/x"}, Args{Ignored: []string{"(flag)"}}},
		{[]string{"-"}, Args{Ignored: []string{"(flag)"}}},
	} {
		if got := ParseArgs(tc.args); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseArgs(%q)\n got %+v\nwant %+v", tc.args, got, tc.want)
		}
	}
}

// TestArgsLogValue: the log shows what was asked for, never a link, a path or an address.
func TestArgsLogValue(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	p := ParseArgs([]string{"--open-url", "isshoni://watch.example.com/r/secret-room", "--self-check=C:\\Users\\x"})
	log.Info("start", "args", p)
	q := ParseArgs([]string{"--self-check=C:\\Users\\x", "--server", "private.example.com", "--updated-from=0.2.0"})
	log.Info("start", "args", q)
	out := buf.String()
	for _, leak := range []string{"secret-room", "watch.example.com", `Users`, "private.example.com"} {
		if strings.Contains(out, leak) {
			t.Errorf("the log holds %q:\n%s", leak, out)
		}
	}
	for _, want := range []string{"args.link=true", "args.mode=--self-check", "args.updated_from=0.2.0", "args.server=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
}

// FuzzParseArgs: whatever the arguments, a launch with --open-url honors nothing but its link (and --relaunched
// first), and the ignored names never carry a value.
func FuzzParseArgs(f *testing.F) {
	f.Add("--open-url\x00isshoni://x/\x00--purge-data")
	f.Add("--purge-data\x00--open-url=isshoni://x/r/y")
	f.Add("--server\x00a.example\x00--version")
	f.Add("--self-check=/tmp/x\x00-Embedding")
	f.Fuzz(func(t *testing.T, joined string) {
		args := strings.Split(joined, "\x00")
		p := ParseArgs(args)
		if p.OpenURL {
			if p.OneShot != "" || p.Server != "" || p.Autostart || p.Debug || p.UpdatedFrom != "" ||
				p.SelfCheckPath != "" || p.SoundCheckHelper {
				t.Fatalf("an --open-url launch honored more than its link: %+v", p)
			}
			if p.Link != "" && !hasIsshoniScheme(p.Link) {
				t.Fatalf("link %q", p.Link)
			}
		}
		for _, name := range p.Ignored {
			if strings.Contains(name, "=") || strings.Contains(name, "://") {
				t.Fatalf("an ignored name carries a value: %q", name)
			}
		}
	})
}
