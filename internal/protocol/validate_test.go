package protocol

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// vcase is one validation case: data is the payload JSON; want is "" for success or "field:reason".
type vcase struct {
	name string
	data string
	want string
}

// runValidation decodes each case through DecodeInto (decode limits, then Validate) into a new value of zero's type.
func runValidation(t *testing.T, zero any, cases []vcase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := reflect.New(reflect.TypeOf(zero))
			err := DecodeInto(Envelope{Type: "x", Data: json.RawMessage(c.data)}, v.Interface())
			got := ""
			if err != nil {
				var fe *FieldError
				if !errors.As(err, &fe) {
					t.Fatalf("error %v (%T) is not a *FieldError", err, err)
				}
				got = fe.Field + ":" + fe.Reason
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// with returns base (a JSON object) with its top-level keys replaced by the pairs in kv (key, raw JSON value; a
// value of "-" deletes the key).
func with(base string, kv ...string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(base), &m); err != nil {
		panic(err)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "-" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = json.RawMessage(kv[i+1])
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

// jsonArray returns a JSON array of n copies of item.
func jsonArray(n int, item string) string {
	return "[" + strings.TrimSuffix(strings.Repeat(item+",", n), ",") + "]"
}

// jsonCodecs returns a JSON array of n distinct well-formed codec keys.
func jsonCodecs(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = jsonString("x" + strings.Repeat("a", i%20) + "/" + string(rune('a'+i%26)))
	}
	return "[" + strings.Join(items, ",") + "]"
}

func TestValidateHello(t *testing.T) {
	const base = `{"protocol":1,"minProtocol":1,"features":[],"client":{"kind":"web","version":"0.1.0","os":"macos",` +
		`"browser":"chrome"},"role":"full","caps":{"decode":["h264/42e0","opus"]}}`
	client := func(fields string) string { return with(base, "client", "{"+fields+"}") }
	runValidation(t, Hello{}, []vcase{
		{"valid", base, ""},
		{"no data", ``, "client.kind:required"},
		{"kind missing", client(`"version":"0.1.0","os":"macos"`), "client.kind:required"},
		{"kind upper case", client(`"kind":"Web"`), "client.kind:invalid"},
		{"kind 17 chars", client(`"kind":"abcdefghijklmnopq"`), "client.kind:invalid"},
		{"kind unknown is accepted", client(`"kind":"watch","os":"other"`), ""},
		{"os upper case", client(`"kind":"web","os":"MacOS"`), "client.os:invalid"},
		{"os empty is accepted", client(`"kind":"web"`), ""},
		{"os unknown is accepted", client(`"kind":"web","os":"haiku"`), ""},
		{"browser with a dash", client(`"kind":"web","browser":"chrome-beta"`), "client.browser:invalid"},
		{"version with a space", client(`"kind":"web","version":"0.1 .0"`), "client.version:invalid"},
		{"version 65 chars", client(`"kind":"web","version":"` + strings.Repeat("1", 65) + `"`), "client.version:invalid"},
		{"version with prerelease", client(`"kind":"tool","version":"0.2.0-dev.3+g1a2b3c"`), ""},
		{"role missing", with(base, "role", "-"), "role:required"},
		{"role unknown", with(base, "role", `"admin"`), "role:invalid"},
		{"role publisher", with(base, "role", `"publisher"`), ""},
		{"role agent", with(base, "role", `"agent"`), ""},
		{"role viewer", with(base, "role", `"viewer"`), ""},
		{"features 32", with(base, "features", jsonArray(32, `"x"`)), ""},
		{"features 33", with(base, "features", jsonArray(33, `"x"`)), "features:too_many"},
		{"features 33, key in upper case", with(base, "FEATURES", jsonArray(33, `"x"`)), "features:too_many"},
		{"features 33, escaped key", strings.Replace(with(base, "features", jsonArray(33, `"x"`)), `"features"`,
			`"feat`+jsonEsc("0075")+`res"`, 1), "features:too_many"},
		{"features 33, key folded from U+017F", strings.Replace(with(base, "features", jsonArray(33, `"x"`)), `"features"`,
			`"feature`+jsonEsc("017f")+`"`, 1), "features:too_many"},
		{"features 33, raw key folded from U+017F", with(base, "feature"+string(rune(0x017f)), jsonArray(33, `"x"`)), "features:too_many"},
		{"unknown feature is accepted", with(base, "features", `["agent.relay","teleport"]`), ""},
		{"decode 32", with(base, "caps", `{"decode":`+jsonCodecs(32)+`}`), ""},
		{"decode 33", with(base, "caps", `{"decode":`+jsonCodecs(33)+`}`), "caps.decode:too_many"},
		{"encode 33", with(base, "caps", `{"decode":[],"encode":`+jsonCodecs(33)+`}`), "caps.encode:too_many"},
		{"decode key upper case", with(base, "caps", `{"decode":["opus","H264/42E0"]}`), "caps.decode[1]:invalid"},
		{"decode key empty", with(base, "caps", `{"decode":[""]}`), "caps.decode[0]:invalid"},
		{"decode key too long", with(base, "caps", `{"decode":["`+strings.Repeat("a", 33)+`"]}`), "caps.decode[0]:invalid"},
		{"decode unknown codec is accepted", with(base, "caps", `{"decode":["av1","vp9","h265/0100"]}`), ""},
		{"encode key invalid", with(base, "caps", `{"decode":[],"encode":["opus!"]}`), "caps.encode[0]:invalid"},
		{"no caps at all", with(base, "caps", "-"), ""},
		{"auth bearer", with(base, "auth", `{"scheme":"bearer","token":"isa_x"}`), ""},
		{"auth scheme missing", with(base, "auth", `{"token":"isa_x"}`), "auth.scheme:required"},
		{"auth scheme unknown", with(base, "auth", `{"scheme":"basic","token":"x"}`), "auth.scheme:invalid"},
		{"auth token missing", with(base, "auth", `{"scheme":"bearer"}`), "auth.token:required"},
		{"auth token 513 bytes", with(base, "auth", `{"scheme":"bearer","token":"`+strings.Repeat("t", 513)+`"}`), "auth.token:too_long"},
		{"protocol versions are not validated", with(base, "protocol", "7", "minProtocol", "5"), ""},
		{"garbage resume token is not an error", with(base, "resumeToken", `"not a token"`), ""},
		{"protocol not a number", with(base, "protocol", `"1"`), "protocol:invalid"},
	})
}

func TestValidateRoomJoin(t *testing.T) {
	for _, c := range []struct {
		data string
		ok   bool
	}{
		{`{"roomId":"lounge"}`, true},
		{`{"roomId":"k3m9p2qxw7ht"}`, true},
		{`{"roomId":"` + strings.Repeat("a", 64) + `"}`, true},
		{`{"roomId":"` + strings.Repeat("a", 65) + `"}`, false},
		{`{"roomId":""}`, false},
		{`{}`, false},
		{``, false},
		{`{"roomId":"no spaces"}`, false},
		{`{"roomId":"../lounge"}`, false},
	} {
		_, err := Decode[RoomJoin](Envelope{Type: MessageTypeRoomJoin, Data: json.RawMessage(c.data)})
		if c.ok {
			if err != nil {
				t.Errorf("%s: %v", c.data, err)
			}
			continue
		}
		// A malformed room id is room_not_found, like 03's REST 404, never bad_request (01 §8.4).
		var pe *Error
		if !errors.As(err, &pe) || pe.Code != ErrorCodeRoomNotFound || pe.Scope != ErrorScopeRequest {
			t.Errorf("%s: got %v, want room_not_found", c.data, err)
		}
	}
}

func TestValidateShareStart(t *testing.T) {
	const base = `{"kind":"screen","preset":"auto","audio":true,"ref":"1s7fq2"}`
	label := func(s string) string { return with(base, "label", jsonString(s)) }
	runValidation(t, ShareStart{}, []vcase{
		{"valid", base, ""},
		{"kind missing", with(base, "kind", "-"), "kind:required"},
		{"kind unknown", with(base, "kind", `"monitor"`), "kind:invalid"},
		{"kinds", with(base, "kind", `"tab"`), ""},
		{"preset missing", with(base, "preset", "-"), "preset:required"},
		{"preset unknown", with(base, "preset", `"cinema"`), "preset:invalid"},
		{"ref missing", with(base, "ref", "-"), "ref:required"},
		{"ref 32", with(base, "ref", jsonString(strings.Repeat("r", 32))), ""},
		{"ref 33", with(base, "ref", jsonString(strings.Repeat("r", 33))), "ref:too_long"},
		{"ref charset", with(base, "ref", `"a/b"`), "ref:invalid"},
		{"replaces", with(base, "replaces", `"s_h4j6k8m0p2r4t6v8"`), ""},
		{"replaces invalid", with(base, "replaces", `"s_h4j6 k8"`), "replaces:invalid"},
		{"replaces too long", with(base, "replaces", jsonString(strings.Repeat("s", 65))), "replaces:too_long"},
		{"label", label("Movie night"), ""},
		{"label 40 code points", label(strings.Repeat("é", 40)), ""},
		{"label 41 code points", label(strings.Repeat("é", 41)), "label:too_long"},
		{"label 40 after trimming", label("  " + strings.Repeat("x", 40) + "\n"), ""},
		{"label emoji", label("🎬 Film"), ""},
		{"label only spaces means none", label("   "), ""},
		{"label control char (Cc)", label("bell\u0007"), "label:invalid"},
		{"label newline inside (Cc)", label("a\nb"), "label:invalid"},
		{"label zero-width space (Cf)", label("a" + string(rune(0x200b)) + "b"), "label:invalid"},
		{"label bidi override (Cf)", label(string(rune(0x202e)) + "gnp.exe"), "label:invalid"},
		{"label line separator (Zl)", label("a" + string(rune(0x2028)) + "b"), "label:invalid"},
		{"label paragraph separator (Zp)", label("a" + string(rune(0x2029)) + "b"), "label:invalid"},
		{"audio not a bool", with(base, "audio", `"yes"`), "audio:invalid"},
		{"invalid UTF-8", "{\"kind\":\"tab\",\"label\":\"\xff\",\"preset\":\"auto\",\"ref\":\"r\"}", "data:invalid"},
	})
}

func TestShareStartTrimsLabel(t *testing.T) {
	s, err := Decode[ShareStart](Envelope{Data: json.RawMessage(
		`{"kind":"window","label":" \t Slides  \n","preset":"text","ref":"r1"}`)})
	if err != nil || s.Label != "Slides" {
		t.Errorf("label %q, %v; want \"Slides\"", s.Label, err)
	}
	s, err = Decode[ShareStart](Envelope{Data: json.RawMessage(`{"kind":"window","label":"  ","preset":"text","ref":"r1"}`)})
	if err != nil || s.Label != "" {
		t.Errorf("blank label %q, %v; want no label", s.Label, err)
	}
	u, err := Decode[ShareUpdate](Envelope{Data: json.RawMessage(`{"shareId":"s_1","label":"  Game  "}`)})
	if err != nil || u.Label == nil || *u.Label != "Game" {
		t.Errorf("update label %v, %v", u.Label, err)
	}
	u, err = Decode[ShareUpdate](Envelope{Data: json.RawMessage(`{"shareId":"s_1","label":""}`)})
	if err != nil || u.Label == nil || *u.Label != "" {
		t.Errorf("clearing label: %v, %v", u.Label, err)
	}
	u, err = Decode[ShareUpdate](Envelope{Data: json.RawMessage(`{"shareId":"s_1"}`)})
	if err != nil || u.Label != nil {
		t.Errorf("absent label: %v, %v", u.Label, err)
	}
}

func TestValidateShareUpdateStop(t *testing.T) {
	runValidation(t, ShareUpdate{}, []vcase{
		{"valid", `{"shareId":"s_q7m2x9c4v8b1n5k3","preset":"movie"}`, ""},
		{"only share id", `{"shareId":"s_q7m2x9c4v8b1n5k3"}`, ""},
		{"share id missing", `{"preset":"movie"}`, "shareId:required"},
		{"share id charset", `{"shareId":"s q"}`, "shareId:invalid"},
		{"preset unknown", `{"shareId":"s_1","preset":"cinema"}`, "preset:invalid"},
		{"label clears", `{"shareId":"s_1","label":""}`, ""},
		{"label invalid", `{"shareId":"s_1","label":"a\u0000b"}`, "label:invalid"},
		{"label too long", `{"shareId":"s_1","label":"` + strings.Repeat("x", 41) + `"}`, "label:too_long"},
	})
	runValidation(t, ShareStop{}, []vcase{
		{"valid", `{"shareId":"s_q7m2x9c4v8b1n5k3"}`, ""},
		{"missing", `{}`, "shareId:required"},
		{"too long", `{"shareId":"` + strings.Repeat("s", 65) + `"}`, "shareId:too_long"},
		{"charset", `{"shareId":"s_q7m2x9c4v8b1n5k3é"}`, "shareId:invalid"},
		{"not a string", `{"shareId":7}`, "shareId:invalid"},
	})
}

func TestValidatePC(t *testing.T) {
	const sdp = `"v=0\r\n"`
	offer := func(pc string, tracks string) string {
		return `{"pc":"` + pc + `","gen":1,"neg":1,"sdp":` + sdp + `,"tracks":` + tracks + `}`
	}
	track := func(mid, share, kind string) string {
		return `{"mid":"` + mid + `","shareId":"` + share + `","kind":"` + kind + `"}`
	}
	tracks := func(n int) string {
		items := make([]string, n)
		for i := range items {
			items[i] = track(strconv.Itoa(i), "s_"+strconv.Itoa(i/2), []string{"video", "audio"}[i%2])
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	bigSDP := jsonString(strings.Repeat("a", MaxSDPBytes+1))
	runValidation(t, PCOffer{}, []vcase{
		{"valid pub", offer("pub", "["+track("0", "s_1", "video")+","+track("1", "s_1", "audio")+"]"), ""},
		{"no tracks", offer("pub", "[]"), ""},
		{"tracks absent", `{"pc":"pub","gen":1,"neg":1,"sdp":` + sdp + `}`, ""},
		{"pc missing", `{"gen":1,"neg":1,"sdp":` + sdp + `}`, "pc:required"},
		{"pc unknown", offer("data", "[]"), "pc:invalid"},
		{"gen 0", `{"pc":"pub","gen":0,"neg":1,"sdp":` + sdp + `}`, "gen:required"},
		{"gen negative", `{"pc":"pub","gen":-1,"neg":1,"sdp":` + sdp + `}`, "gen:invalid"},
		{"gen too big", `{"pc":"pub","gen":4294967296,"neg":1,"sdp":` + sdp + `}`, "gen:invalid"},
		{"neg 0", `{"pc":"pub","gen":1,"neg":0,"sdp":` + sdp + `}`, "neg:required"},
		{"sdp missing", `{"pc":"pub","gen":1,"neg":1}`, "sdp:required"},
		{"sdp too long", `{"pc":"pub","gen":1,"neg":1,"sdp":` + bigSDP + `}`, "sdp:too_long"},
		{"pub 8 tracks", offer("pub", tracks(8)), ""},
		{"pub 9 tracks", offer("pub", tracks(9)), "tracks:too_many"},
		{"sub 512 tracks", offer("sub", tracks(512)), ""},
		{"sub 513 tracks", offer("sub", tracks(513)), "tracks:too_many"},
		{"duplicate mid", offer("pub", "["+track("0", "s_1", "video")+","+track("0", "s_1", "audio")+"]"), "tracks:duplicate"},
		{"pub: two videos of one share", offer("pub", "["+track("0", "s_1", "video")+","+track("1", "s_1", "video")+"]"), "tracks:duplicate"},
		{"pub: two audios of one share", offer("pub", "["+track("0", "s_1", "audio")+","+track("1", "s_1", "audio")+"]"), "tracks:duplicate"},
		{"pub: two shares", offer("pub", "["+track("0", "s_1", "video")+","+track("1", "s_2", "video")+","+track("2", "s_2", "audio")+"]"), ""},
		{"sub: the server's mapping is not constrained", offer("sub", "["+track("0", "s_1", "video")+","+track("1", "s_1", "video")+"]"), ""},
		{"mid missing", offer("pub", "["+track("", "s_1", "video")+"]"), "tracks:required"},
		{"mid with space", offer("pub", "["+track("a b", "s_1", "video")+"]"), "tracks:invalid"},
		{"share missing", offer("pub", "["+track("0", "", "video")+"]"), "tracks:required"},
		{"share invalid", offer("pub", "["+track("0", "s 1", "video")+"]"), "tracks:invalid"},
		{"kind missing", offer("pub", "["+track("0", "s_1", "")+"]"), "tracks:required"},
		{"kind unknown", offer("pub", "["+track("0", "s_1", "data")+"]"), "tracks:invalid"},
	})
	runValidation(t, PCAnswer{}, []vcase{
		{"valid", `{"pc":"sub","gen":2,"neg":1,"sdp":` + sdp + `}`, ""},
		{"pc missing", `{"gen":2,"neg":1,"sdp":` + sdp + `}`, "pc:required"},
		{"gen 0", `{"pc":"sub","neg":1,"sdp":` + sdp + `}`, "gen:required"},
		{"neg 0", `{"pc":"sub","gen":2,"sdp":` + sdp + `}`, "neg:required"},
		{"sdp missing", `{"pc":"sub","gen":2,"neg":1}`, "sdp:required"},
		{"sdp too long", `{"pc":"sub","gen":2,"neg":1,"sdp":` + bigSDP + `}`, "sdp:too_long"},
	})
	cand := func(fields string) string { return `{"pc":"pub","gen":1,"candidate":{` + fields + `}}` }
	runValidation(t, PCICE{}, []vcase{
		{"valid", cand(`"candidate":"candidate:1 1 udp 1 192.0.2.1 1 typ host","sdpMid":"0","sdpMLineIndex":0`), ""},
		{"end of candidates", `{"pc":"pub","gen":1}`, ""},
		{"empty candidate string", cand(`"candidate":""`), ""},
		{"candidate 512 bytes", cand(`"candidate":"` + strings.Repeat("c", 512) + `"`), ""},
		{"candidate 513 bytes", cand(`"candidate":"` + strings.Repeat("c", 513) + `"`), "candidate.candidate:too_long"},
		{"sdpMid too long", cand(`"candidate":"c","sdpMid":"` + strings.Repeat("m", 33) + `"`), "candidate.sdpMid:too_long"},
		{"ufrag too long", cand(`"candidate":"c","usernameFragment":"` + strings.Repeat("u", 257) + `"`), "candidate.usernameFragment:too_long"},
		{"sdpMLineIndex overflow", cand(`"candidate":"c","sdpMLineIndex":70000`), "candidate.sdpMLineIndex:invalid"},
		{"pc missing", `{"gen":1}`, "pc:required"},
		{"gen missing", `{"pc":"sub"}`, "gen:required"},
	})
	runValidation(t, PCRestart{}, []vcase{
		{"valid ice", `{"pc":"sub","gen":1,"mode":"ice","reason":"disconnected"}`, ""},
		{"valid rebuild", `{"pc":"pub","gen":3,"mode":"rebuild","reason":"failed"}`, ""},
		{"mode missing", `{"pc":"sub","gen":1,"reason":"failed"}`, "mode:required"},
		{"mode unknown", `{"pc":"sub","gen":1,"mode":"full","reason":"failed"}`, "mode:invalid"},
		{"reason missing", `{"pc":"sub","gen":1,"mode":"ice"}`, "reason:required"},
		{"reason codec does not exist", `{"pc":"sub","gen":1,"mode":"rebuild","reason":"codec"}`, "reason:invalid"},
		{"gen 0", `{"pc":"sub","gen":0,"mode":"ice","reason":"failed"}`, "gen:required"},
	})
	runValidation(t, PCClose{}, []vcase{
		{"valid", `{"pc":"pub","gen":1}`, ""},
		{"sub is accepted (the hub ignores it)", `{"pc":"sub","gen":1}`, ""},
		{"gen 0", `{"pc":"pub"}`, "gen:required"},
		{"gen a string", `{"pc":"pub","gen":"1"}`, "gen:invalid"},
	})
}

func TestValidateSubscribeUpdate(t *testing.T) {
	sub := func(share, video, audio string) string {
		return `{"shareId":"` + share + `","video":"` + video + `","audio":"` + audio + `"}`
	}
	subs := func(n int) string {
		items := make([]string, n)
		for i := range items {
			items[i] = sub("s_"+strings.Repeat("x", i/10+1)+string(rune('0'+i%10)), "low", "off")
		}
		return `{"subs":[` + strings.Join(items, ",") + `]}`
	}
	runValidation(t, SubscribeUpdate{}, []vcase{
		{"valid", `{"subs":[` + sub("s_1", "high", "on") + `,` + sub("s_2", "off", "off") + `]}`, ""},
		{"empty", `{"subs":[]}`, "subs:required"},
		{"absent", `{}`, "subs:required"},
		{"64", subs(64), ""},
		{"65", subs(65), "subs:too_many"},
		{"65 with the key in another case", strings.Replace(subs(65), `"subs"`, `"SUBS"`, 1), "subs:too_many"},
		{"share id missing", `{"subs":[` + sub("", "high", "on") + `]}`, "subs[0].shareId:required"},
		{"share id invalid", `{"subs":[` + sub("s_1", "low", "on") + `,` + sub("s 2", "low", "on") + `]}`, "subs[1].shareId:invalid"},
		{"video missing", `{"subs":[` + sub("s_1", "", "on") + `]}`, "subs[0].video:required"},
		{"video mid is later (M5)", `{"subs":[` + sub("s_1", "mid", "on") + `]}`, "subs[0].video:invalid"},
		{"audio missing", `{"subs":[` + sub("s_1", "low", "") + `]}`, "subs[0].audio:required"},
		{"audio unknown", `{"subs":[` + sub("s_1", "low", "loud") + `]}`, "subs[0].audio:invalid"},
		{"duplicate", `{"subs":[` + sub("s_1", "low", "on") + `,` + sub("s_2", "low", "on") + `,` + sub("s_1", "high", "off") + `]}`,
			"subs[2].shareId:duplicate"},
		{"not an array", `{"subs":{"shareId":"s_1"}}`, "subs:invalid"},
	})
}

func TestValidateCapsUpdate(t *testing.T) {
	runValidation(t, CapsUpdate{}, []vcase{
		{"valid", `{"caps":{"decode":["h264/42e0","opus"]}}`, ""},
		{"empty decode", `{"caps":{"decode":[]}}`, ""},
		{"decode 33", `{"caps":{"decode":` + jsonCodecs(33) + `}}`, "caps.decode:too_many"},
		{"encode 33", `{"caps":{"encode":` + jsonCodecs(33) + `}}`, "caps.encode:too_many"},
		{"CAPS.Decode 33", `{"CAPS":{"Decode":` + jsonCodecs(33) + `}}`, "caps.decode:too_many"},
		{"invalid key", `{"caps":{"decode":["h264 42e0"]}}`, "caps.decode[0]:invalid"},
		{"decode under another key is not limited", `{"other":{"decode":` + jsonCodecs(40) + `}}`, ""},
	})
}

func TestValidateClientStats(t *testing.T) {
	in := func(fields string) string { return `{"intervalMs":10000,"pcs":[],"inbound":[{` + fields + `}]}` }
	many := func(key string, n int, item string) string {
		return `{"intervalMs":10000,"` + key + `":` + jsonArray(n, item) + `}`
	}
	runValidation(t, ClientStats{}, []vcase{
		{"valid", `{"intervalMs":10000,"pcs":[{"pc":"sub","gen":1,"state":"connected","rttMs":38}]}`, ""},
		{"interval negative", `{"intervalMs":-1,"pcs":[]}`, "intervalMs:invalid"},
		{"pcs 4", many("pcs", 4, `{"pc":"pub","gen":1,"state":"new"}`), ""},
		{"pcs 5", many("pcs", 5, `{"pc":"pub","gen":1,"state":"new"}`), "pcs:too_many"},
		{"inbound 129", many("inbound", 129, `{"shareId":"s_1","kind":"video","bitrate":1,"packetsLost":0}`), "inbound:too_many"},
		{"outbound 17", many("outbound", 17, `{"shareId":"s_1","kind":"video","bitrate":1}`), "outbound:too_many"},
		{"pc kind unknown", `{"intervalMs":1,"pcs":[{"pc":"x","gen":1,"state":"new"}]}`, "pcs[0].pc:invalid"},
		{"pc kind missing", `{"intervalMs":1,"pcs":[{"pc":"sub","gen":1},{"gen":1,"state":"new"}]}`, "pcs[1].pc:required"},
		{"pc rtt negative", `{"intervalMs":1,"pcs":[{"pc":"sub","gen":1,"state":"new","rttMs":-3}]}`, "pcs[0]:invalid"},
		{"pc state too long", `{"intervalMs":1,"pcs":[{"pc":"sub","gen":1,"state":"` + strings.Repeat("s", 65) + `"}]}`, "pcs[0]:too_long"},
		{"inbound valid", in(`"shareId":"s_1","kind":"audio","bitrate":128000,"packetsLost":-2,"totalSamples":10`), ""},
		{"inbound share id missing", in(`"kind":"audio","bitrate":1,"packetsLost":0`), "inbound[0].shareId:required"},
		{"inbound kind unknown", in(`"shareId":"s_1","kind":"data","bitrate":1,"packetsLost":0`), "inbound[0].kind:invalid"},
		{"inbound kind missing", in(`"shareId":"s_1","bitrate":1,"packetsLost":0`), "inbound[0].kind:required"},
		{"inbound negative counter", in(`"shareId":"s_1","kind":"video","bitrate":1,"packetsLost":0,"framesDecoded":-5`), "inbound[0]:invalid"},
		{"inbound negative bitrate", in(`"shareId":"s_1","kind":"video","bitrate":-1,"packetsLost":0`), "inbound[0]:invalid"},
		{"inbound decoder too long", in(`"shareId":"s_1","kind":"video","bitrate":1,"packetsLost":0,"decoder":"` + strings.Repeat("d", 65) + `"`), "inbound[0]:too_long"},
		{"outbound negative fps", `{"intervalMs":1,"pcs":[],"outbound":[{"shareId":"s_1","kind":"video","bitrate":1,"fps":-1}]}`, "outbound[0]:invalid"},
		{"outbound kind missing", `{"intervalMs":1,"pcs":[],"outbound":[{"shareId":"s_1","bitrate":1}]}`, "outbound[0].kind:required"},
		{"outbound kind unknown", `{"intervalMs":1,"pcs":[],"outbound":[{"shareId":"s_1","kind":"data","bitrate":1}]}`, "outbound[0].kind:invalid"},
	})
	runValidation(t, StatsWatch{}, []vcase{
		{"on", `{"on":true}`, ""},
		{"off", `{}`, ""},
		{"not a bool", `{"on":1}`, "on:invalid"},
	})
}

func TestValidateAgentSend(t *testing.T) {
	const base = `{"toRole":"agent","kind":"share.request","payload":{"preset":"game"}}`
	runValidation(t, AgentSend{}, []vcase{
		{"valid by role", base, ""},
		{"valid by connection", with(base, "toRole", "-", "to", `"c_k3v9q2m7xw4pa8d1"`), ""},
		{"no target", with(base, "toRole", "-"), "to:required"},
		{"both targets", with(base, "to", `"c_1"`), "toRole:invalid"},
		{"bad connection id", with(base, "toRole", "-", "to", `"c 1"`), "to:invalid"},
		{"unknown role", with(base, "toRole", `"admin"`), "toRole:invalid"},
		{"kind missing", with(base, "kind", "-"), "kind:required"},
		{"kind upper case", with(base, "kind", `"Share.Request"`), "kind:invalid"},
		{"kind with a digit", with(base, "kind", `"share2"`), "kind:invalid"},
		{"kind 33", with(base, "kind", jsonString(strings.Repeat("k", 33))), "kind:too_long"},
		{"payload missing", with(base, "payload", "-"), "payload:required"},
		// null is no payload (01 §5: nothing the hub sends has a null of its own). It is the one value refused.
		{"payload null", with(base, "payload", "null"), "payload:required"},
		{"payload null, spaced", `{"toRole":"agent","kind":"share.request","payload" : ` + "\n\tnull }", "payload:required"},
		{"payload empty object", with(base, "payload", `{}`), ""},
		{"payload array is opaque", with(base, "payload", `[1,2,3]`), ""},
		{"payload with a null inside is opaque", with(base, "payload", `{"preset":null,"list":[null]}`), ""},
		{"payload [null] is opaque", with(base, "payload", `[null]`), ""},
		{"payload \"null\" is a string", with(base, "payload", `"null"`), ""},
		{"payload false", with(base, "payload", `false`), ""},
		{"payload 0", with(base, "payload", `0`), ""},
		{"payload empty string", with(base, "payload", `""`), ""},
		{"payload 16 KiB", with(base, "payload", jsonString(strings.Repeat("p", MaxAgentPayloadBytes-2))), ""},
	})
	// A null that never went through the decoder, as a Go caller may build it, is refused all the same; a payload
	// that was left out encodes as {} (TestNilRelayPayloadEncodesAsObject) and passes.
	for _, raw := range []string{"null", " null", "null\n", "\t null \r\n"} {
		a := AgentSend{ToRole: RoleAgent, Kind: "share.request", Payload: json.RawMessage(raw)}
		var fe *FieldError
		if err := a.Validate(); !errors.As(err, &fe) || fe.Field != "payload" || fe.Reason != FieldRequired {
			t.Errorf("payload %q: got %v, want payload:required", raw, err)
		}
	}
	built, err := json.Marshal(AgentSend{ToRole: RoleAgent, Kind: "share.request"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode[AgentSend](Envelope{Type: MessageTypeAgentSend, Data: built}); err != nil {
		t.Errorf("an agent.send built without a payload (%s): %v", built, err)
	}
	// A payload over 16 KiB is message_too_large, not bad_request (01 §13, P12): a *Error, like room_not_found.
	for _, n := range []int{MaxAgentPayloadBytes - 1, 17 << 10} {
		data := with(base, "payload", jsonString(strings.Repeat("p", n)))
		_, err := Decode[AgentSend](Envelope{Type: MessageTypeAgentSend, Data: json.RawMessage(data)})
		var pe *Error
		if !errors.As(err, &pe) || pe.Code != ErrorCodeMessageTooLarge || pe.Scope != ErrorScopeRequest || pe.Retryable {
			t.Errorf("payload of %d bytes: got %#v, want message_too_large, scope request, not retryable", n+2, err)
		}
	}
}

// TestDecodeRejectsDeepNesting: a payload nested deeper than 32 levels is rejected before encoding/json sees it.
func TestDecodeRejectsDeepNesting(t *testing.T) {
	deep := func(n int) string {
		return `{"payload":` + strings.Repeat("[", n) + strings.Repeat("]", n) + `}`
	}
	if _, err := Decode[AgentRecv](Envelope{Data: json.RawMessage(deep(maxDepth - 1))}); err != nil {
		t.Errorf("depth %d: %v", maxDepth, err)
	}
	_, err := Decode[AgentRecv](Envelope{Data: json.RawMessage(deep(maxDepth))})
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Field != "data" || fe.Reason != FieldInvalid {
		t.Errorf("depth %d: got %v, want data:invalid", maxDepth+1, err)
	}
}

// TestDecodeAbsentData: absent data and data: null decode as the zero value, and Validate still runs.
func TestDecodeAbsentData(t *testing.T) {
	if _, err := Decode[Empty](Envelope{Type: MessageTypeRoomLeave}); err != nil {
		t.Errorf("Empty: %v", err)
	}
	if p, err := Decode[Ping](Envelope{Type: MessageTypePing, Data: json.RawMessage("null")}); err != nil || p.T != 0 {
		t.Errorf("Ping from null: %v %v", p, err)
	}
	_, err := Decode[ShareStop](Envelope{Type: MessageTypeShareStop})
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Field != "shareId" {
		t.Errorf("ShareStop without data: %v", err)
	}
	if err := DecodeInto(Envelope{Data: json.RawMessage(`{}`)}, Ping{}); err == nil {
		t.Error("DecodeInto a non-pointer succeeded")
	}
	var fe2 *FieldError
	if err := DecodeInto(Envelope{Data: json.RawMessage(`{"t":`)}, &Ping{}); !errors.As(err, &fe2) {
		t.Errorf("malformed data: %v", err)
	}
}

// TestValidateMethodsExist: every client->server payload of the Registry has a Validate method (01 §15.1), except
// the ones with nothing to validate.
func TestValidateMethodsExist(t *testing.T) {
	noValidate := map[reflect.Type]bool{reflect.TypeFor[Ping](): true, reflect.TypeFor[Empty](): true}
	for _, s := range Registry {
		if s.Dir != DirClientToServer {
			continue
		}
		typ := reflect.TypeOf(s.Payload)
		_, has := reflect.New(typ).Interface().(validator)
		if has == noValidate[typ] {
			t.Errorf("%s: %v has Validate = %v", s.Type, typ, has)
		}
	}
}

func TestFieldErrorBadRequest(t *testing.T) {
	fe := &FieldError{Field: "subs[3].video", Reason: FieldInvalid}
	e := fe.BadRequest(ErrorScopePC)
	want := Error{Code: ErrorCodeBadRequest, Scope: ErrorScopePC, Params: map[string]any{"field": "subs[3].video", "reason": "invalid"}}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("got %#v", e)
	}
	if got := fe.Error(); got != "protocol: field subs[3].video: invalid" {
		t.Errorf("Error() = %q", got)
	}
}
