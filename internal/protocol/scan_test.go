package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// parseEnvelopeJSON is ParseEnvelope as it was before the envelope scanner: encoding/json reads the frame into the
// struct. It is the reference that the scanner must agree with, for every input: the same frames accepted, and the
// same envelope for each of them.
func parseEnvelopeJSON(b []byte) (Envelope, error) {
	var e Envelope
	if !utf8.Valid(b) {
		return Envelope{}, fmt.Errorf("%w: not valid UTF-8", ErrBadMessage)
	}
	if err := scanJSON(b, nil); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	switch {
	case e.Type == "":
		return Envelope{}, fmt.Errorf("%w: missing type", ErrBadMessage)
	case len(e.Type) > maxTypeLen:
		return Envelope{}, fmt.Errorf("%w: type too long", ErrBadMessage)
	case e.ID != "" && !validToken(e.ID, MaxIDLen):
		return Envelope{}, fmt.Errorf("%w: invalid id", ErrBadMessage)
	case e.Re != "" && !validToken(e.Re, MaxIDLen):
		return Envelope{}, fmt.Errorf("%w: invalid re", ErrBadMessage)
	}
	switch {
	case len(e.Data) == 0:
	case bytes.Equal(e.Data, jsonNull):
		e.Data = nil
	case e.Data[0] != '{':
		return Envelope{}, fmt.Errorf("%w: data is not an object", ErrBadMessage)
	}
	return e, nil
}

// parseLikeJSON runs ParseEnvelope on b and fails the test unless the result is the reference's: an error for the
// same frames (one that wraps ErrBadMessage, with a zero Envelope), and otherwise the same fields, byte for byte.
func parseLikeJSON(t testing.TB, b []byte) (Envelope, error) {
	t.Helper()
	got, err := ParseEnvelope(b)
	want, wantErr := parseEnvelopeJSON(b)
	switch {
	case (err == nil) != (wantErr == nil):
		t.Fatalf("ParseEnvelope(%q) = %+v, %v; encoding/json gives %+v, %v", b, got, err, want, wantErr)
	case err != nil && !errors.Is(err, ErrBadMessage):
		t.Fatalf("ParseEnvelope(%q): error %v does not wrap ErrBadMessage", b, err)
	case err != nil && !reflect.DeepEqual(got, Envelope{}):
		t.Fatalf("ParseEnvelope(%q) returned %+v with its error", b, got)
	case got.Type != want.Type || got.ID != want.ID || got.Re != want.Re,
		!bytes.Equal(got.Data, want.Data) || (got.Data == nil) != (want.Data == nil):
		t.Fatalf("ParseEnvelope(%q) = %+v (data %q); encoding/json gives %+v (data %q)", b, got, got.Data, want, want.Data)
	}
	return got, err
}

// jsonValues are JSON values and near misses of every kind: what the scanner has to check wherever a frame holds a
// value that the envelope does not keep.
func jsonValues() []string {
	u := jsonEsc
	return []string{
		// Numbers.
		`0`, `-0`, `1`, `-1`, `10`, `1.5`, `1.50`, `0.0`, `-0.0e-0`, `1e5`, `1E5`, `1e+5`, `1e-5`, `1.5e10`, `0e0`, `1E400`,
		`-1E-400`, `123456789012345678901234567890`, `0.000000000000000000000000000001`,
		`01`, `00`, `-01`, `-`, `--1`, `+1`, `.5`, `-.5`, `1.`, `0.`, `1.e5`, `0.e1`, `1e`, `1E`, `1e+`, `1e-`, `1ee5`, `1e++5`,
		`1e5.5`, `1.5.5`, `0x10`, `1_000`, `1a`, `- 1`, `1 .5`, `1 e5`, `NaN`, `Infinity`, `-Infinity`, `1,5`,
		// Literals.
		`true`, `false`, `null`,
		`tru`, `True`, `TRUE`, `nul`, `nulll`, `Null`, `fals`, `falsee`, `t`, `f`, `n`, `truefalse`, `true false`, `nil`,
		`undefined`,
		// Strings.
		`""`, `"a"`, `"a b"`, `"é"`, `"日本"`, `"😀"`, "\"a\x7fb\"", `"{}[],:"`, `"\"\\\/\b\f\n\r\t"`, `"\\"`, `"\\\\"`, `"\""`,
		`"` + u("0041") + `"`, `"` + u("00e9") + u("abCD") + `"`, `"` + u("d83d") + u("de00") + `"`, `"` + u("d800") + `"`,
		`"` + u("dc00") + `"`, `"` + u("d800") + `a"`, `"` + u("dc00") + u("d800") + `"`, `"` + u("0000") + `"`,
		`"`, `"a`, `"a\"`, `"a\`, `"\x"`, `"\'"`, `"\a"`, `"\v"`, `"\0"`, `"\U00000041"`, `"\ "`,
		`"` + u("12") + `"`, `"` + u("123g") + `"`, `"` + u("") + `"`, `"` + u("00") + `zz"`, `"` + u("d83d") + `\"`, `"` + u("004"),
		"\"a\tb\"", "\"a\nb\"", "\"a\x00b\"", "\"a\x1fb\"", `'a'`, `"a""b"`, `"a" "b"`,
		// Arrays.
		`[]`, `[ ]`, `[1]`, `[1,2]`, `[ 1 , 2 ]`, `[[]]`, `[[],[]]`, `[{}]`, `["a",true,null,-1.5e3,{"a":[]}]`,
		`[`, `]`, `[1`, `[1,`, `[1,]`, `[,1]`, `[,]`, `[1 2]`, `[1;2]`, `[1,,2]`, `[}`, `[1}`, `[[]`, `[]]`, `["a":1]`, `[1]]`,
		// Objects.
		`{}`, `{ }`, `{"a":1}`, `{ "a" : 1 }`, `{"a":1,"b":2}`, `{"a":1,"a":2}`, `{"":1}`, `{"a":{}}`, `{"a":{"b":[1,{"c":null}]}}`,
		`{"` + u("0061") + `":1}`, `{"a\"b":1}`, `{"type":5,"id":[],"re":{},"data":"x"}`,
		`{`, `}`, `{"a"`, `{"a":`, `{"a":1`, `{"a":1,`, `{"a":1,}`, `{,}`, `{,"a":1}`, `{"a"}`, `{"a":}`, `{:1}`, `{1:1}`, `{a:1}`,
		`{'a':1}`, `{"a":1 "b":2}`, `{"a"::1}`, `{"a":1,,"b":2}`, `{"a":1;"b":2}`, `{"a" 1}`, `{"a":1:2}`, `{]`, `{"a":1]`,
		`{{}}`, `{"a":{}`, `{}}`, `{"a":1}}`, `{null:1}`, `{"a\x":1}`, "{\"a\tb\":1}", `{"a`,
		// Whitespace and what JSON does not count as whitespace, around a value.
		" 1 ", "\t\r\n1\n\r\t", "\v1", "1\v", "\f1", "1\f", "\xc2\xa01", "1\xc2\xa0", "\xef\xbb\xbf1", "1\x00", "/**/1", "1//", "1 2",
		``, ` `,
	}
}

// envelopeFrames are frames that exercise each rule by which encoding/json reads an Envelope (scan.go lists
// them), and each way a frame can be malformed. They are the seeds of FuzzParseEnvelope too.
func envelopeFrames() []string {
	u := jsonEsc
	rep := strings.Repeat
	frames := []string{
		// Keys: matched without regard to case, after unescaping; nothing else matches.
		`{"TYPE":"a"}`, `{"Type":"a"}`, `{"tYpE":"a","ID":"1","Re":"2","DATA":{"k":1}}`, `{"type":"a","iD":"b","rE":"c","dAtA":{}}`,
		`{"ty` + u("0070") + `e":"a"}`, `{"` + u("0054") + `YPE":"a"}`, `{"` + u("0074") + u("0079") + u("0070") + u("0065") + `":"a"}`,
		`{"type":"a","` + u("0069") + `d":"1","r` + u("0045") + `":"2","dat` + u("0061") + `":{"k":1}}`,
		`{"type":"a","` + u("0049") + u("0044") + `":"1"}`, `{"t\ype":"a"}`, `{"type":"a","i\/d":"1"}`,
		`{"ıd":"1","type":"a"}`, `{"İd":"1","type":"a"}`, `{"type":"a","ⅰd":"1"}`, `{"ｔｙｐｅ":"a"}`, `{"τype":"a"}`,
		`{"type":"a","ſ":"1","K":"2"}`, `{"type":"a","da_ta":{"k":1}}`, `{"type":"a","da-ta":{"k":1}}`, `{"ty-pe":"a"}`,
		`{"type ":"a"}`, `{" type":"a"}`, `{"typ":"a"}`, `{"types":"a"}`, `{"":"a"}`, `{"":"a","type":"b"}`, `{"t":"a","type":"b"}`,
		`{"type` + u("0000") + `":"a"}`, `{"` + u("0000") + `type":"a"}`, `{"type\n":"a"}`, `{"\"type\"":"a"}`, `{"type":"a","id\\":"1"}`,
		`{"` + u("d83d") + u("de00") + `":"a","type":"b"}`, `{"` + u("d800") + `":"a","type":"b"}`, `{"` + u("d800") + `type":"a"}`,
		`{"` + rep("type", 20) + `":"a","type":"b"}`, `{"` + rep(u("0074"), 70) + `":"a","type":"b"}`, `{"type` + rep("e", 70) + u("0065") + `":"a"}`,
		`{"` + rep("x", 70) + `\n":"a","type":"b"}`, `{"` + rep(u("d83d")+u("de00"), 20) + `":"a","type":"b"}`,
		// Repeated keys: the last value counts.
		`{"type":"a","type":"b"}`, `{"type":"a","TYPE":"b"}`, `{"TYPE":"b","type":"a"}`, `{"type":"a","type":""}`, `{"type":"","type":"a"}`,
		`{"type":"` + rep("a", 100) + `","type":"b"}`, `{"type":"b","type":"` + rep("a", 100) + `"}`,
		`{"type":"a","id":"1","id":"2"}`, `{"type":"a","id":"1","id":""}`, `{"type":"a","id":"bad id","id":"2"}`, `{"type":"a","id":"2","id":"bad id"}`,
		`{"type":"a","re":"1","Re":"2","RE":"3"}`, `{"type":"a","data":{"a":1},"data":{"b":2}}`, `{"type":"a","data":{"a":1},"data":null}`,
		`{"type":"a","data":null,"data":{"a":1}}`, `{"type":"a","data":[1],"data":{"a":1}}`, `{"type":"a","data":{"a":1},"data":[1]}`,
		`{"type":"a","data":"x","Data":null}`, `{"type":"a","data":{},"data":{}}`,
		// null leaves a string field as it is; any other value that is not a string is an error, wherever it stands.
		`{"type":null}`, `{"type":"a","type":null}`, `{"type":null,"type":"a"}`, `{"type":"a","id":null}`, `{"type":"a","id":"1","id":null}`,
		`{"type":"a","re":null,"re":"1","re":null}`, `{"type":"a","id":null,"re":null,"data":null}`,
		`{"type":5,"type":"a"}`, `{"type":"a","type":5}`, `{"type":"a","id":1}`, `{"type":"a","id":"1","id":1}`, `{"type":"a","id":1,"id":"1"}`,
		`{"type":"a","re":true}`, `{"type":"a","re":false}`, `{"type":"a","re":[]}`, `{"type":"a","re":{}}`, `{"type":"a","re":1.5}`,
		`{"type":["a"]}`, `{"type":{"type":"a"}}`, `{"type":true}`, `{"type":"a","id":["1"]}`, `{"type":"a","TYPE":1}`,
		// String values: escapes are decoded, lengths are those of the decoded text.
		`{"type":"` + u("0070") + `ing"}`, `{"type":"p` + u("0069") + u("006e") + u("0067") + `","id":"` + u("0031") + `","re":"` + u("005f") + u("002d") + `"}`,
		`{"type":"a\"b"}`, `{"type":"\"\\\/\b\f\n\r\t"}`, `{"type":"a` + u("0000") + `b"}`, `{"type":"é😀"}`, `{"type":"` + u("00e9") + `"}`,
		`{"type":"` + u("d83d") + u("de00") + `"}`, `{"type":"` + u("d800") + `"}`, `{"type":"` + u("dc00") + `"}`, `{"type":"` + u("d800") + `a"}`,
		`{"type":"` + u("d800") + u("0061") + `"}`, `{"type":"` + u("dc00") + u("d800") + `"}`, `{"type":"` + u("d83d") + u("d83d") + u("de00") + `"}`,
		`{"type":"` + rep("a", 64) + `"}`, `{"type":"` + rep("a", 65) + `"}`, `{"type":"` + rep(u("0061"), 64) + `"}`, `{"type":"` + rep(u("0061"), 65) + `"}`,
		`{"type":"` + rep("a", 63) + u("0061") + `"}`, `{"type":"` + rep("a", 64) + u("0061") + `"}`, `{"type":"` + u("0061") + rep("a", 64) + `"}`,
		`{"type":"` + rep("😀", 16) + `"}`, `{"type":"` + rep("😀", 17) + `"}`, `{"type":"a` + rep("😀", 16) + `"}`,
		`{"type":"` + rep(u("d83d")+u("de00"), 16) + `"}`, `{"type":"` + rep(u("d83d")+u("de00"), 17) + `"}`, `{"type":"aaa` + rep(u("d83d")+u("de00"), 15) + `a"}`,
		`{"type":"aaa` + rep(u("d83d")+u("de00"), 15) + `aa"}`, `{"type":"` + rep(u("d800"), 21) + `a"}`, `{"type":"` + rep(u("d800"), 21) + `aa"}`,
		`{"type":"` + rep("é", 32) + `"}`, `{"type":"a` + rep("é", 32) + `"}`, `{"type":"` + rep(u("00e9"), 32) + `"}`, `{"type":"a` + rep(u("00e9"), 32) + `"}`,
		`{"type":"` + rep("a", 300) + u("0061") + `"}`, `{"type":"` + u("0061") + rep("a", 300) + `"}`, `{"type":"` + rep(u("0061"), 300) + `"}`,
		`{"type":"a","id":"` + rep("a", 32) + `"}`, `{"type":"a","id":"` + rep("a", 33) + `"}`, `{"type":"a","id":"` + rep(u("0061"), 32) + `"}`,
		`{"type":"a","id":"` + rep(u("0061"), 33) + `"}`, `{"type":"a","re":"` + rep("a", 31) + u("0061") + `"}`, `{"type":"a","re":"` + rep("a", 32) + u("0061") + `"}`,
		`{"type":"a","id":"aZ09_-"}`, `{"type":"a","id":"a b"}`, `{"type":"a","id":"a` + u("0020") + `b"}`, `{"type":"a","id":"é"}`, `{"type":"a","id":"` + u("00e9") + `"}`,
		`{"type":"a","id":"1.2"}`, `{"type":"a","re":"a/b"}`, `{"type":"a","re":"a\/b"}`, `{"type":"a","id":""}`, `{"type":"a","re":""}`, `{"type":""}`,
		`{"type":"a","id":"` + u("0000") + `"}`, `{"type":"a","id":"` + u("d800") + `"}`, `{"type":"a","id":"` + rep("a", 70) + `"}`, `{"type":"a","id":"` + rep(u("0061"), 70) + `"}`,
		// data: an object or null, as the bytes of the frame.
		`{"type":"a","data":{}}`, `{"type":"a","data": { "k" : [ 1 , 2 ] } }`, "{\"type\":\"a\",\"data\":\t{\n\"k\":1\r}\n}", `{"type":"a","data":null}`,
		`{"type":"a","data":[]}`, `{"type":"a","data":"{}"}`, `{"type":"a","data":0}`, `{"type":"a","data":true}`, `{"type":"a","data":false}`,
		`{"type":"a","data":nul}`, `{"type":"a","data":nulll}`, `{"type":"a","data":{"type":5,"id":[],"re":{},"data":"x"}}`,
		`{"type":"a","data":{"k":"` + u("0041") + `","` + u("0042") + `":2}}`, `{"data":{"k":1}}`, `{"data":{"k":1},"type":"a"}`,
		// The frame: one object, nothing but JSON whitespace around it.
		`{}`, `{ }`, ` {"type":"a"} `, "\t\r\n{\"type\":\"a\"}\n\r\t", "{\n\t\"type\" : \"a\" ,\r\n\t\"id\" : \"1\"\n}", `{"type":"a"}}`, `{"type":"a"}{}`,
		`{"type":"a"} x`, `{"type":"a"},`, `{"type":"a"}null`, "{\"type\":\"a\"}\x00", "{\"type\":\"a\"}\f", "{\"type\":\"a\"}\v", "\xef\xbb\xbf{\"type\":\"a\"}",
		"{\"type\":\"a\"}\xc2\xa0", "{\"type\":\"a\"\v}", "{\"type\"\f:\"a\"}", `{"type":"a"/**/}`, `{"type":"a",}`, `{,"type":"a"}`, `{"type":"a" "id":"1"}`,
		`{"type":"a";"id":"1"}`, `{"type" "a"}`, `{"type"="a"}`, `{"type":"a","id"}`, `{"type":"a","id":}`, `{"type":"a",,"id":"1"}`, `{'type':"a"}`,
		`{type:"a"}`, `{"type":'a'}`, `{"type":"a"`, `{"type":"a",`, `{"type":"a","`, `{"type":"a","id`, `{"type":"a","id"`, `{"type":"a","id":`, `{"type":"a","id":"`,
		`{"type":"a","id":"1`, `{"type":"a","id":"1"`, `{"type`, `{"`, `{`, `}`, `{]`, `[{"type":"a"}]`, `{"type":"a"}]`, `[]`, `"a"`, `"type"`, `1`, `true`, `null`,
		` null `, `nul`, ``, ` `, "\n", `{"type":"a\`, `{"type":"a\"`, `{"type":"a` + u("00") + `"}`, `{"ty` + u("00") + `pe":"a"}`, "{\"ty\tpe\":\"a\"}",
		`{"type":"a","x":1}{"type":"b"}`, `{"type":"a"}` + rep(" ", 300), rep(" ", 300) + `{"type":"a"}`, `{{"type":"a"}}`,
	}
	// A value of every kind in a key that the envelope ignores.
	for _, v := range jsonValues() {
		frames = append(frames, `{"type":"a","u":`+v+`}`)
	}
	// Nesting: 32 levels with the envelope's own, in data and in a key that the envelope ignores.
	for _, n := range []int{1, 30, 31, 32, 64, 1000} {
		arrays := rep("[", n) + rep("]", n)
		objects := rep(`{"a":`, n) + `1` + rep("}", n)
		mixed := rep(`[{"a":`, n/2) + `[]` + rep("}]", n/2)
		frames = append(frames,
			`{"type":"a","u":`+arrays+`}`, `{"type":"a","u":`+objects+`}`, `{"type":"a","u":`+mixed+`}`,
			`{"type":"a","data":{"k":`+arrays+`}}`, `{"type":"a","data":`+objects+`}`,
			`{"type":"a","u":`+rep("[", n), `{"type":"a","u":`+rep(`{"a":`, n), `{"type":"a","u":`+arrays[:len(arrays)-1]+`}}`,
			arrays, objects,
		)
	}
	return append(frames, longKeyFrames(2574)...)
}

// longKeyFrames are frames of n bytes that are mostly one object key, whole and broken off: what F04 was about.
// encoding/json allocated 3 to 13 times the key for them (a ~ or a / in the key costs it the most: it escapes
// them for the JSON Pointer of its error).
func longKeyFrames(n int) []string {
	var out []string
	for _, shape := range []struct{ before, after string }{
		{`{"`, `":`}, // a frame that breaks off after a long key: what FuzzParseEnvelope found (the seed in testdata)
		{`{"`, `"`}, {`{"`, ``}, {`{"`, `":1`}, {`{"`, `":1,`}, {`{"`, `":x}`}, {`{"`, `"x`}, {`{"`, `":1}`}, {`{"`, `":"a","type":"b"}`},
		{`{"type":"a","`, `":`}, {`{"type":"a","`, `":1}`}, {`{"type":"a","`, `":{"`}, {`{"type":"a","u":[{"`, `":[`},
		{`{"type":"a","data":{"`, `":`}, {`{"type":"a","data":{"`, `":1}}`}, {`{"type":"a","data":{"k":{"`, `":{"a":`},
		{`{"type":"room.join","id":"2","data":{"roomId":"lounge","`, `":`},
		{`{"type":"a","id":"1","`, `":1,"data":{"k":1}}`},
	} {
		for _, fill := range []string{"k", "~", "/", "é", "日", "😀", `\n`, `\\`, jsonEsc("0041"), jsonEsc("d83d") + jsonEsc("de00")} {
			room := n - len(shape.before) - len(shape.after)
			key := strings.Repeat(fill, room/len(fill))
			out = append(out, shape.before+strings.Repeat("k", room-len(key))+key+shape.after)
		}
	}
	return out
}

// TestParseEnvelopeLikeEncodingJSON: the scanner reads every frame as encoding/json did.
func TestParseEnvelopeLikeEncodingJSON(t *testing.T) {
	frames := envelopeFrames()
	// A value of every kind, also where the envelope keeps one, deeper in the frame, in a frame that breaks off, in
	// the place of a key, and as the whole frame.
	for _, v := range jsonValues() {
		frames = append(frames,
			`{"u":`+v+`,"type":"a"}`,
			`{"type":"a","data":{"k":`+v+`}}`,
			`{"type":"a","data":{"k":[`+v+`,`+v+`],"l":{"m":`+v+`}}}`,
			`{"type":`+v+`}`,
			`{"type":"a","id":`+v+`}`,
			`{"type":"a","data":`+v+`}`,
			`{"type":"a","u":`+v,
			`{"type":"a","u":[`+v,
			`{"type":"a",`+v+`:1}`,
			v,
		)
	}
	accepted := 0
	for _, in := range frames {
		if _, err := parseLikeJSON(t, []byte(in)); err == nil {
			accepted++
		}
	}
	// The table must not be one-sided: a reference that rejected everything would agree with any scanner.
	if accepted < len(frames)/10 || accepted > len(frames)*9/10 {
		t.Errorf("%d of %d frames are accepted; the table should hold plenty of both kinds", accepted, len(frames))
	}
}

// TestParseEnvelopeRules spells out the rules of scan.go with their results, so they do not rest on the
// reference alone.
func TestParseEnvelopeRules(t *testing.T) {
	u := jsonEsc
	type want struct {
		typ, id, re, data string
	}
	for _, c := range []struct {
		name, in string
		want     *want // nil: rejected
	}{
		{"keys in any case", `{"TYPE":"a","Id":"1","rE":"2","DATA":{"k":1}}`, &want{"a", "1", "2", `{"k":1}`}},
		{"escaped key", `{"ty` + u("0070") + `e":"a","` + u("0049") + `d":"1"}`, &want{typ: "a", id: "1"}},
		{"dashes and underscores are not ignored", `{"type":"a","i_d":"1","r-e":"2","da_ta":[]}`, &want{typ: "a"}},
		{"look-alike letters are other keys", `{"type":"a","ıd":"1","İD":"2"}`, &want{typ: "a"}},
		{"last type counts", `{"type":"a","Type":"b"}`, &want{typ: "b"}},
		{"last type counts, also an empty one", `{"type":"a","type":""}`, nil},
		{"a too long type is fine when it is not the last", `{"type":"` + strings.Repeat("a", 100) + `","type":"b"}`, &want{typ: "b"}},
		{"an invalid id is fine when it is not the last", `{"type":"a","id":"bad id","id":"2"}`, &want{typ: "a", id: "2"}},
		{"last data counts", `{"type":"a","data":[1],"data":{"b":2}}`, &want{typ: "a", data: `{"b":2}`}},
		{"last data counts, also null", `{"type":"a","data":{"a":1},"data":null}`, &want{typ: "a"}},
		{"last data counts, also an array", `{"type":"a","data":{"a":1},"data":[1]}`, nil},
		{"null leaves type", `{"type":"a","type":null}`, &want{typ: "a"}},
		{"null leaves id and re", `{"type":"a","id":"1","re":"2","id":null,"re":null}`, &want{typ: "a", id: "1", re: "2"}},
		{"null is no type", `{"type":null}`, nil},
		{"a number is no type, even before a string", `{"type":5,"type":"a"}`, nil},
		{"an object is no id", `{"type":"a","id":{}}`, nil},
		{"false is no re", `{"type":"a","re":false}`, nil},
		{"escapes in values", `{"type":"p` + u("0069") + `ng","id":"` + u("0031") + `","re":"` + u("005F") + `"}`, &want{"ping", "1", "_", ""}},
		{"a surrogate pair", `{"type":"` + u("d83d") + u("de00") + `"}`, &want{typ: "😀"}},
		{"half a surrogate pair", `{"type":"` + u("d800") + `"}`, &want{typ: string(utf8.RuneError)}},
		{"type of 64 bytes, escaped", `{"type":"` + strings.Repeat(u("0061"), 64) + `"}`, &want{typ: strings.Repeat("a", 64)}},
		{"type of 65 bytes, escaped", `{"type":"` + strings.Repeat(u("0061"), 65) + `"}`, nil},
		{"type of 64 bytes in 16 runes", `{"type":"` + strings.Repeat(u("d83d")+u("de00"), 16) + `"}`, &want{typ: strings.Repeat("😀", 16)}},
		{"type of 68 bytes in 17 runes", `{"type":"` + strings.Repeat(u("d83d")+u("de00"), 17) + `"}`, nil},
		{"id of 32 bytes, escaped", `{"type":"a","id":"` + strings.Repeat(u("0061"), 32) + `"}`, &want{typ: "a", id: strings.Repeat("a", 32)}},
		{"id of 33 bytes, escaped", `{"type":"a","id":"` + strings.Repeat(u("0061"), 33) + `"}`, nil},
		{"id with an escaped space", `{"type":"a","id":"1` + u("0020") + `2"}`, nil},
		{"data keeps its bytes", `{"type":"a","data": { "k" : "` + u("0041") + `" } }`, &want{typ: "a", data: `{ "k" : "` + u("0041") + `" }`}},
		{"keys inside data are data", `{"type":"a","data":{"type":5,"id":[],"data":"x"}}`, &want{typ: "a", data: `{"type":5,"id":[],"data":"x"}`}},
		{"number with a leading zero", `{"type":"a","u":01}`, nil},
		{"control character in a string", "{\"type\":\"a\",\"u\":\"a\tb\"}", nil},
		{"unknown escape", `{"type":"a","u":"\x"}`, nil},
		{"single quote escape", `{"type":"a","u":"\'"}`, nil},
		{"trailing comma", `{"type":"a",}`, nil},
		{"vertical tab is not whitespace", "{\"type\":\"a\"}\v", nil},
		{"byte order mark", "\xef\xbb\xbf{\"type\":\"a\"}", nil},
		{"two objects", `{"type":"a"}{}`, nil},
		{"a long key the envelope ignores", `{"` + strings.Repeat("~", 3000) + `":{"type":1},"type":"a"}`, &want{typ: "a"}},
		{"a frame that breaks off after a long key", `{"type":"a","` + strings.Repeat("k", 3000) + `":`, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, err := parseLikeJSON(t, []byte(c.in))
			if c.want == nil {
				if err == nil {
					t.Fatalf("accepted: %+v", e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := (want{string(e.Type), e.ID, e.Re, string(e.Data)}); got != *c.want {
				t.Errorf("got %+v, want %+v", got, *c.want)
			}
			if (e.Data == nil) != (c.want.data == "") {
				t.Errorf("data = %q; absent data must be nil, and only that", e.Data)
			}
		})
	}
}

// mutationPieces are what a mutation puts in: JSON's own characters, and pieces that make or break its tokens.
var mutationPieces = []string{
	`{`, `}`, `[`, `]`, `"`, `:`, `,`, `\`, ` `, "\n", "\t", "\x00", `0`, `1`, `-`, `.`, `e`, `E`, `+`, `a`, `n`, `t`, `f`, `u`, `é`, `😀`,
	`null`, `true`, `false`, `""`, `{}`, `[]`, `"type":`, `"id":`, `"re":`, `"data":`, `"TYPE":"z",`, `"Data":null,`, `"id":"7",`,
	`"x":1,`, `,"x":[{}]`, jsonEsc("0041"), jsonEsc("d83d"), jsonEsc("de00"), jsonEsc("00"), `\n`, `\"`, `\/`, `\x`,
}

// newMutations returns the source of a test's mutations: the same ones in every run.
func newMutations(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, 2026)) //nolint:gosec // G404: the same mutations in every run, not a secret
}

// mutateJSON returns a copy of b that is broken in one to three small ways: a deterministic sample of what the
// fuzzer does.
func mutateJSON(rng *rand.Rand, b []byte) []byte {
	b = bytes.Clone(b)
	for n := 1 + rng.IntN(3); n > 0 && len(b) > 0; n-- {
		at := rng.IntN(len(b))
		piece := mutationPieces[rng.IntN(len(mutationPieces))]
		switch rng.IntN(6) {
		case 0: // insert
			b = append(b[:at:at], append([]byte(piece), b[at:]...)...)
		case 1: // replace one byte
			b = append(b[:at:at], append([]byte(piece), b[at+1:]...)...)
		case 2: // delete one byte
			b = append(b[:at:at], b[at+1:]...)
		case 3: // break off
			b = b[:at]
		case 4: // delete a stretch
			end := min(len(b), at+1+rng.IntN(12))
			b = append(b[:at:at], b[end:]...)
		case 5: // double a stretch
			end := min(len(b), at+1+rng.IntN(12))
			b = append(b[:end:end], b[at:]...)
		}
	}
	return b
}

// TestParseEnvelopeMutations compares the scanner with encoding/json on the golden fixtures, each broken in many
// small ways, in every run of the tests.
func TestParseEnvelopeMutations(t *testing.T) {
	rounds := 400
	if testing.Short() {
		rounds = 40
	}
	rng := newMutations(4)
	accepted, rejected := 0, 0
	for _, fx := range loadFixtures(t, fixtureDir) {
		for range rounds {
			b := mutateJSON(rng, fx.raw)
			if !utf8.Valid(b) {
				continue // cutting through a rune: rejected before the scanner runs
			}
			if _, err := parseLikeJSON(t, b); err == nil {
				accepted++
			} else {
				rejected++
			}
		}
	}
	if accepted < 100 || rejected < 100 {
		t.Errorf("%d mutations accepted, %d rejected; the sample should hold plenty of both kinds", accepted, rejected)
	}
}

// TestEnvelopeScanKnowsEveryField holds the scanner's keys to the Envelope struct: a field added to the struct
// would otherwise be skipped as an unknown key.
func TestEnvelopeScanKnowsEveryField(t *testing.T) {
	typ := reflect.TypeFor[Envelope]()
	seen := map[*[]byte]string{}
	var s envelopeScan
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Errorf("Envelope.%s has no JSON name", typ.Field(i).Name)
			continue
		}
		for _, key := range []string{name, strings.ToUpper(name)} {
			dst := s.field([]byte(key), false)
			if dst == nil {
				t.Errorf("the scanner skips the key %q of Envelope.%s", key, typ.Field(i).Name)
			} else if other, ok := seen[dst]; ok && other != name {
				t.Errorf("the scanner stores %q where it stores %q", key, other)
			}
			seen[dst] = name
		}
	}
	if len(seen) != 4 {
		t.Errorf("the scanner knows %d fields (type, id, re, data); Envelope has %d", len(seen), typ.NumField())
	}
	for _, key := range []string{"", "t", "typ", "types", "i", "ids", "r", "rex", "dat", "datas", "payload"} {
		if s.field([]byte(key), false) != nil {
			t.Errorf("the scanner takes %q for a key of the envelope", key)
		}
	}
}

// TestParseEnvelopeAllocations: ParseEnvelope allocates its result and nothing else. What the envelope does not
// keep costs no memory, however long it is, and a rejected frame costs none at all (F04: a frame that broke off
// after a 2.5 KB key allocated 19 KB in encoding/json).
func TestParseEnvelopeAllocations(t *testing.T) {
	long := strings.Repeat("k", 2570)
	for _, c := range []struct {
		name, in string
		ok       bool
		// One per field of the result that is longer than a byte (the runtime has the 1-byte strings ready).
		allocs float64
	}{
		{"breaks off after a long key", `{"` + long + `":`, false, 0},
		{"breaks off after a long key in data", `{"type":"room.join","id":"42","data":{"roomId":"lounge","` + long + `":`, false, 0},
		{"breaks off inside a long key", `{"type":"a","` + long, false, 0},
		{"long key with slashes, then garbage", `{"type":"a","` + strings.Repeat("/~", 1285) + `":x}`, false, 0},
		{"long escaped key, no type", `{"` + strings.Repeat(jsonEsc("0041"), 400) + `":1}`, false, 0},
		{"too deep", `{"type":"a","id":"1","data":{"k":` + strings.Repeat("[", 31) + strings.Repeat("]", 31) + `}}`, false, 0},
		{"not UTF-8", "{\"type\":\"a\",\"" + long + "\":\"\xff\"}", false, 0},
		{"not an object", `["` + long + `"]`, false, 0},
		{"long type", `{"type":"` + long + `"}`, false, 0},
		{"long escaped type", `{"type":"` + strings.Repeat(jsonEsc("0061"), 400) + `"}`, false, 0},
		{"invalid id after a type", `{"type":"room.join","id":"` + long + `","data":{"roomId":"lounge"}}`, false, 0},
		{"invalid re after a type and an id", `{"type":"ok","id":"1","re":"a b","data":{"k":1}}`, false, 0},
		{"data is an array", `{"type":"room.join","id":"2","data":["` + long + `"]}`, false, 0},
		{"type is a number", `{"id":"2","data":{"k":"` + long + `"},"type":5}`, false, 0},
		{"type only", `{"type":"ping"}`, true, 1},
		{"long key that the envelope ignores", `{"type":"ping","` + long + `":{"a":[1,2,"` + long + `"]}}`, true, 1},
		{"long keys of every kind", `{"` + strings.Repeat("~/", 1285) + `":1,"type":"ping","` + strings.Repeat(jsonEsc("0041"), 400) + `":2}`, true, 1},
		{"request", `{"type":"room.join","id":"42","data":{"roomId":"lounge"}}`, true, 3},
		{"request with a long key in data", `{"type":"room.join","id":"42","data":{"roomId":"lounge","` + long + `":1}}`, true, 3},
		{"reply with escapes", `{"type":"o` + jsonEsc("006b") + `","re":"4` + jsonEsc("0032") + `","data":null}`, true, 2},
		{"repeated keys", `{"type":"` + long + `","type":"ok","re":"41","re":"42","data":{"a":"` + long + `"},"data":{}}`, true, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := []byte(c.in)
			e, err := parseLikeJSON(t, b)
			if (err == nil) != c.ok {
				t.Fatalf("ParseEnvelope = %+v, %v; want ok=%v", e, err, c.ok)
			}
			if got := testing.AllocsPerRun(100, func() { _, _ = ParseEnvelope(b) }); got != c.allocs {
				t.Errorf("%v allocations, want %v", got, c.allocs)
			}
			// And in bytes: the result's own, each field rounded up to the allocator's next size.
			limit := uint64(len(e.Type)+len(e.ID)+len(e.Re)+len(e.Data))*5/4 + 64
			if n := allocatedWithin(limit, func() { _, _ = ParseEnvelope(b) }); n > limit {
				t.Errorf("allocated %d bytes for a result of %d and a frame of %d", n, len(e.Type)+len(e.ID)+len(e.Re)+len(e.Data), len(b))
			}
		})
	}
}

// TestSkipValue: the scanner's value check on its own, against encoding/json's.
func TestSkipValue(t *testing.T) {
	for _, in := range jsonValues() {
		for _, raw := range [][]byte{nil, []byte("a")} {
			if b := []byte(in); utf8.Valid(b) && validPayload(b, raw) != json.Valid(b) {
				t.Errorf("validPayload(%q, %q) = %v; json.Valid = %v", in, raw, validPayload(b, raw), json.Valid(b))
			}
		}
	}
	// Depth counts the containers around the value too.
	nested := []byte(strings.Repeat("[", 4) + strings.Repeat("]", 4))
	if _, err := skipValue(nested, 0, maxDepth-4, 0); err != nil {
		t.Errorf("4 levels inside %d: %v", maxDepth-4, err)
	}
	if _, err := skipValue(nested, 0, maxDepth-3, 0); !errors.Is(err, errTooDeep) || !errors.Is(err, ErrBadMessage) {
		t.Errorf("4 levels inside %d: error %v, want errTooDeep in ErrBadMessage", maxDepth-3, err)
	}
	// It stops after the value, wherever that is.
	if end, err := skipValue([]byte(`[1,{"a":"]"}] ,2`), 0, 1, 0); err != nil || end != 13 {
		t.Errorf("skipValue stopped at %d, %v; want 13", end, err)
	}
	// A limit on the names holds for every object in the value, for the bytes between the quotes, and for nothing
	// but names.
	for _, c := range []struct {
		in      string
		maxName int
		ok      bool
	}{
		{`{"abc":1}`, 0, true}, {`{"abc":1}`, 3, true}, {`{"abc":1}`, 2, false},
		{`{"a":1,"abc":1}`, 2, false}, {`{"a":{"abc":1}}`, 2, false}, {`[1,[{"a":1},{"a":1,"abc":[]}]]`, 2, false},
		{`{"a":"abcdef","b":["abcdef"]}`, 2, true}, {`"abcdef"`, 2, true}, {`{"":"abc"}`, 1, true},
		{`{"a` + jsonEsc("0062") + `":1}`, 7, true}, {`{"a` + jsonEsc("0062") + `":1}`, 6, false}, {`{"é":1}`, 2, true}, {`{"é":1}`, 1, false},
	} {
		end, err := skipValue([]byte(c.in), 0, 0, c.maxName)
		if (err == nil) != c.ok || c.ok && end != len(c.in) {
			t.Errorf("skipValue(%s) with names of at most %d bytes: %d, %v; want ok=%v", c.in, c.maxName, end, err, c.ok)
		}
	}
}

// unmarshalDataJSON is unmarshalData as it was before it checked a payload's syntax itself: encoding/json finds the
// syntax errors. It is the reference of TestDecodeSyntaxCheck.
func unmarshalDataJSON(data json.RawMessage, v any) error {
	if len(data) == 0 || bytes.Equal(data, jsonNull) {
		return nil
	}
	if !utf8.Valid(data) {
		return &FieldError{Field: "data", Reason: FieldInvalid}
	}
	if err := scanJSON(data, arrayLimitsOf(v)); err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			return fe
		}
		return &FieldError{Field: "data", Reason: FieldInvalid}
	}
	if err := json.Unmarshal(data, v); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) && ute.Field != "" {
			return &FieldError{Field: ute.Field, Reason: FieldInvalid}
		}
		return &FieldError{Field: "data", Reason: FieldInvalid}
	}
	return nil
}

func arrayLimitsOf(v any) *limitNode {
	if l, ok := v.(arrayLimiter); ok {
		return l.arrayLimits()
	}
	return nil
}

// rawMemberOf returns the JSON name of the member that v, a pointer to a payload, keeps as bytes, or "".
func rawMemberOf(v any) string {
	if r, ok := v.(rawMember); ok {
		return string(r.rawMember())
	}
	return ""
}

// longKey reports whether data, which is valid JSON, has a member name that Decode refuses: one of more than
// maxKeyLen bytes between its quotes, outside the value of the payload object's member raw ("": no such member). It
// reads the tokens with encoding/json's Decoder, so it is a reference for the scanner's own count.
func longKey(t testing.TB, data []byte, raw string) bool {
	t.Helper()
	type level struct{ object, name bool } // name: the next token of this object is a member's name
	var stack []level
	inRaw := false // inside the value of the payload's member raw
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // a number may be too large for a float64
	for {
		before := dec.InputOffset()
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			t.Fatalf("the tokens of %q: %v", data, err)
		}
		top := len(stack) - 1
		switch d, isDelim := tok.(json.Delim); {
		case isDelim && (d == '{' || d == '['):
			stack = append(stack, level{object: d == '{', name: d == '{'})
			continue
		case isDelim:
			stack = stack[:top]
		case top >= 0 && stack[top].object && stack[top].name:
			stack[top].name = false
			// The name as the message spells it: from its quote to the next quote that no backslash escapes.
			text := bytes.TrimLeft(data[before:dec.InputOffset()], " \t\r\n,:")
			n := 1
			for ; text[n] != '"'; n++ {
				if text[n] == '\\' {
					n++
				}
			}
			if n-1 > maxKeyLen && !inRaw {
				return true
			}
			if top == 0 && raw != "" && strings.EqualFold(tok.(string), raw) {
				inRaw = true
			}
			continue
		}
		// A value has ended: in an object a name comes next, and back in the payload object the member is over.
		if top = len(stack) - 1; top >= 0 && stack[top].object {
			stack[top].name = true
		}
		if top == 0 {
			inRaw = false
		}
	}
}

// TestLongKey holds the tests' reference for the limit on keys to a few cases by hand.
func TestLongKey(t *testing.T) {
	at, over := strings.Repeat("k", maxKeyLen), strings.Repeat("k", maxKeyLen+1)
	for _, c := range []struct {
		data, raw string
		want      bool
	}{
		{`{"` + at + `":1}`, "", false}, {`{"` + over + `":1}`, "", true}, {` { "a" : [ { "b" : 1 , "` + over + `" : 2 } ] } `, "", true},
		{`{"a":"` + over + `","b":["` + over + `"]}`, "", false}, {`["` + over + `",{"a":{"` + at + `":"` + over + `"}}]`, "", false},
		{`{"` + strings.Repeat(jsonEsc("0041"), maxKeyLen/6) + `":1}`, "", false}, {`{"` + strings.Repeat(jsonEsc("0041"), maxKeyLen/6+1) + `":1}`, "", true},
		{`{"` + strings.Repeat(`\"`, maxKeyLen/2) + `":1}`, "", false}, {`{"` + strings.Repeat(`\"`, maxKeyLen/2) + `k":1}`, "", true},
		{`{"payload":{"` + over + `":1}}`, "", true}, {`{"payload":{"` + over + `":[{"` + over + `":1}]}}`, "payload", false},
		{`{"PayLoad":{"` + over + `":1},"p` + jsonEsc("0061") + `yload":[[{"` + over + `":1}]]}`, "payload", false},
		{`{"payload":{"` + over + `":1},"a":{"` + over + `":1}}`, "payload", true}, {`{"payload":1,"a":{"` + over + `":1}}`, "payload", true},
		{`{"a":{"payload":{"` + over + `":1}}}`, "payload", true}, {`[{"payload":{"` + over + `":1}}]`, "payload", true},
		{`{"payload":{"` + over + `":1},"` + over + `":1}`, "payload", true}, {`{"payloads":{"` + over + `":1}}`, "payload", true},
	} {
		if got := longKey(t, []byte(c.data), c.raw); got != c.want {
			t.Errorf("longKey(%.60q..., %q) = %v, want %v", c.data, c.raw, got, c.want)
		}
	}
}

// TestDecodeSyntaxCheck: Decode checks a payload before encoding/json reads it, and that changes no result but the
// ones it is there to change.
//   - A payload that is not valid JSON is refused as before, with the field "data", only without encoding/json's
//     error, which costs many times the length of a long key.
//   - A payload with a key of more than maxKeyLen bytes is refused the same way. Before, it was decoded, and the key
//     ignored if the type did not know it, at up to 5 times the key's length. The keys inside the payload of an
//     agent.send or agent.recv are not limited.
//   - Every other payload decodes to what it decoded to before, or fails with the same error.
//
// (Every payload that the hub decodes has passed ParseEnvelope and is valid JSON.)
func TestDecodeSyntaxCheck(t *testing.T) {
	rounds := 200
	if testing.Short() {
		rounds = 20
	}
	dataInvalid := FieldError{Field: "data", Reason: FieldInvalid}
	valid, malformed, long := 0, 0, 0
	check := func(zero any, data []byte) {
		t.Helper()
		got, want := reflect.New(reflect.TypeOf(zero)), reflect.New(reflect.TypeOf(zero))
		err, wantErr := unmarshalData(data, got.Interface()), unmarshalDataJSON(data, want.Interface())
		var fe, wantFE *FieldError
		switch {
		case len(data) == 0: // absent data: the zero value
			if err != nil || wantErr != nil || !reflect.DeepEqual(got.Elem().Interface(), zero) {
				t.Fatalf("%T from no data: %+v, %v", zero, got.Elem().Interface(), err)
			}
		case utf8.Valid(data) && scanJSON(data, arrayLimitsOf(want.Interface())) == nil &&
			(!json.Valid(data) || longKey(t, data, rawMemberOf(want.Interface()))):
			if json.Valid(data) {
				long++
			} else {
				malformed++
			}
			if !errors.As(err, &fe) || *fe != dataInvalid {
				t.Fatalf("%T from %.200q: %v, want data: invalid", zero, data, err)
			}
			if !reflect.DeepEqual(got.Elem().Interface(), zero) {
				t.Fatalf("%T from %.200q: the refused payload left %+v behind", zero, data, got.Elem().Interface())
			}
		case (err == nil) != (wantErr == nil), errors.As(err, &fe) != errors.As(wantErr, &wantFE), fe != nil && *fe != *wantFE:
			t.Fatalf("%T from %.200q: %v; before the syntax check: %v", zero, data, err, wantErr)
		case !reflect.DeepEqual(got.Interface(), want.Interface()):
			t.Fatalf("%T from %.200q:\n got %+v\nwant %+v", zero, data, got.Elem().Interface(), want.Elem().Interface())
		default:
			if json.Valid(data) {
				valid++
			}
		}
	}

	rng := newMutations(5)
	for _, fx := range loadFixtures(t, fixtureDir) {
		zero, _, err := fixturePayload(fx)
		if err != nil || len(fx.env.Data) == 0 {
			continue
		}
		check(zero, fx.env.Data)
		for range rounds {
			check(zero, mutateJSON(rng, fx.env.Data))
		}
	}
	// Values of every kind and long keys, as the payload of every type and inside one: where the type has a member
	// of that name, where it has none, and in the member that the relay's types keep as bytes.
	at, over, long8k := strings.Repeat("k", maxKeyLen), strings.Repeat("k", maxKeyLen+1), strings.Repeat("~/", 4000)
	values := append(jsonValues(),
		`{"`+long8k+`":`, `{"`+long8k+`":1}`, `{"`+long8k+`":{"subs":`, `{"`+long8k+`":x}`,
		`{"`+at+`":1}`, `{"`+over+`":1}`, `{"`+at+`":[{"`+at+`":{}},{"a":1,"`+over+`":null}]}`, `[{"`+over+`":1}]`, `"`+over+`"`,
		`{"`+strings.Repeat(jsonEsc("0041"), maxKeyLen/6)+`":1}`, `{"`+strings.Repeat(jsonEsc("0041"), maxKeyLen/6+1)+`":1}`,
	)
	for _, tg := range decodeTargets() {
		zero := reflect.Zero(tg.typ).Interface()
		for _, v := range values {
			check(zero, []byte(v))
			check(zero, []byte(`{"u":`+v+`}`))
			check(zero, []byte(`{"subs":[`+v+`],"roomId":`+v+`,"caps":{"decode":`+v+`}}`))
			check(zero, []byte(`{"kind":"a.b","payload":`+v+`}`))
			check(zero, []byte(`{"PAYLOAD":[`+v+`],"u":1,"p`+jsonEsc("0061")+`yload":{"k":`+v+`}}`))
		}
	}
	if valid < 1000 || malformed < 1000 || long < 500 {
		t.Errorf("%d valid payloads, %d malformed ones, %d with a long key; the sample should hold plenty of each kind", valid, malformed, long)
	}
}

// TestDecodeKeyLength spells out the limit on an object key's length with its results, so it does not rest on the
// reference alone.
func TestDecodeKeyLength(t *testing.T) {
	at, over := strings.Repeat("k", maxKeyLen), strings.Repeat("k", maxKeyLen+1)
	agent := `"toRole":"agent","kind":"a.b"`
	for _, c := range []struct {
		name string
		zero any
		data string
		ok   bool
	}{
		{"a key at the limit", RoomJoin{}, `{"` + at + `":1,"roomId":"lounge"}`, true},
		{"a key over the limit", RoomJoin{}, `{"` + over + `":1,"roomId":"lounge"}`, false},
		{"after the keys that the type knows", RoomJoin{}, `{"roomId":"lounge","` + over + `":1}`, false},
		{"in an element of a list", SubscribeUpdate{}, `{"subs":[{"shareId":"s_1","` + over + `":1}]}`, false},
		{"under a key that the type does not know", RoomJoin{}, `{"roomId":"lounge","x":[{"` + over + `":1}]}`, false},
		{"escapes count as they are written", RoomJoin{}, `{"` + strings.Repeat(jsonEsc("0041"), maxKeyLen/6+1) + `":1,"roomId":"lounge"}`, false},
		{"the same key, written out", RoomJoin{}, `{"` + strings.Repeat("A", maxKeyLen/6+1) + `":1,"roomId":"lounge"}`, true},
		{"a long value is no key", RoomJoin{}, `{"roomId":"lounge","x":"` + over + `","y":["` + over + `"]}`, true},
		{"a payload member means nothing to this type", RoomJoin{}, `{"roomId":"lounge","payload":{"` + over + `":1}}`, false},
		{"agent.send: keys inside the payload", AgentSend{}, `{` + agent + `,"payload":{"` + over + `":{"` + over + over + `":[{"` + over + `":1}]}}}`, true},
		{"agent.send: the payload in capitals", AgentSend{}, `{` + agent + `,"PAYLOAD":{"` + over + `":1}}`, true},
		{"agent.send: the payload with an escape", AgentSend{}, `{` + agent + `,"p` + jsonEsc("0061") + `yload":[{"` + over + `":1}]}`, true},
		{"agent.send: every payload of several", AgentSend{}, `{"payload":{"` + over + `":1},` + agent + `,"payload":{"` + over + `":2}}`, true},
		{"agent.send: a key beside the payload", AgentSend{}, `{` + agent + `,"payload":{"a":1},"` + over + `":1}`, false},
		{"agent.send: a key in another member, after the payload", AgentSend{}, `{"payload":{"` + over + `":1},` + agent + `,"x":{"` + over + `":1}}`, false},
		{"agent.send: a payload inside another member", AgentSend{}, `{` + agent + `,"payload":{"a":1},"x":{"payload":{"` + over + `":1}}}`, false},
		{"agent.recv: keys inside the payload", AgentRecv{}, `{"from":"c_1","kind":"a.b","payload":{"` + over + `":1}}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			typ := reflect.TypeOf(c.zero)
			got, want := reflect.New(typ), reflect.New(typ)
			err := DecodeInto(Envelope{Data: []byte(c.data)}, got.Interface())
			if !c.ok {
				var fe *FieldError
				if !errors.As(err, &fe) || fe.Field != "data" || fe.Reason != FieldInvalid {
					t.Fatalf("DecodeInto: %v, want data: invalid", err)
				}
				if !reflect.DeepEqual(got.Elem().Interface(), c.zero) {
					t.Errorf("the refused payload left %+v behind", got.Elem().Interface())
				}
				return
			}
			// Accepted, and decoded as encoding/json alone decodes it.
			if err != nil {
				t.Fatal(err)
			}
			if err := unmarshalDataJSON([]byte(c.data), want.Interface()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Interface(), want.Interface()) || reflect.DeepEqual(got.Elem().Interface(), c.zero) {
				t.Errorf("got %+v, want %+v", got.Elem().Interface(), want.Elem().Interface())
			}
		})
	}

	// The envelope has no such limit: the frame is no bad_message (which would end the connection), and the request
	// in it is answered with bad_request.
	frame := []byte(`{"type":"room.join","id":"7","` + over + `":1,"data":{"roomId":"lounge","` + over + `":1}}`)
	e, err := parseLikeJSON(t, frame)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	var fe *FieldError
	if _, err := Decode[RoomJoin](e); !errors.As(err, &fe) || fe.Field != "data" || fe.Reason != FieldInvalid {
		t.Errorf("Decode: %v, want data: invalid", err)
	}
	// The relay hands on a payload with long keys as it came.
	payload := `{"` + over + `":[{"` + over + `":"x"}]}`
	e, err = parseLikeJSON(t, []byte(`{"type":"agent.send","id":"8","data":{"toRole":"agent","kind":"a.b","payload":`+payload+`}}`))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if a, err := Decode[AgentSend](e); err != nil || string(a.Payload) != payload {
		t.Errorf("Decode: %v, payload %.40q...; want the payload as it came", err, a.Payload)
	}
}

// TestRawMembers holds rawMember to the payload types: a type says which member it keeps as bytes, and a type with
// such a member says so. (A type that named a member which encoding/json does read would lift the limit on the
// keys in there.)
func TestRawMembers(t *testing.T) {
	seen := 0
	for _, tg := range decodeTargets() {
		var raws []string
		for i := range tg.typ.NumField() {
			if f := tg.typ.Field(i); f.Type == reflect.TypeFor[json.RawMessage]() {
				name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				raws = append(raws, name)
			}
		}
		switch name := rawMemberOf(reflect.New(tg.typ).Interface()); {
		case name == "" && len(raws) > 0:
			t.Errorf("%v keeps %q as bytes and has no rawMember method: the keys in there are held to %d bytes", tg.typ, raws, maxKeyLen)
		case name != "" && (len(raws) != 1 || raws[0] != name):
			t.Errorf("%v: rawMember names %q; its json.RawMessage members are %q", tg.typ, name, raws)
		case name != "":
			seen++
		}
	}
	if seen != 2 {
		t.Errorf("%d payload types have a raw member, want 2 (agent.send, agent.recv)", seen)
	}
}

// TestDecodeLongKeyAllocations: keys that a payload's type does not know stay within Decode's allocation bound
// (01 §19, the bound of FuzzDecode), for every payload that the hub decodes. One long key is refused for the price
// of the error. A message full of keys at the limit is decoded, with the keys of the lengths that cost
// encoding/json the most per byte: just over a power of two, where its folded copy has just been grown.
func TestDecodeLongKeyAllocations(t *testing.T) {
	fill := func(keyLen, size int) []byte {
		var b bytes.Buffer
		b.WriteString(`{"roomId":"lounge"`)
		for n := 0; b.Len() < size; n++ {
			fmt.Fprintf(&b, `,"%04d%s":1`, n, strings.Repeat("k", keyLen-4))
		}
		b.WriteString(`}`)
		return b.Bytes()
	}
	payloads := map[string][]byte{
		"one long key":       []byte(`{"` + strings.Repeat("k", 100000) + `":1,"roomId":"lounge"}`),
		"one key of 250 KiB": []byte(`{"roomId":"lounge","` + strings.Repeat("k", 250<<10) + `":{"a":1}}`),
	}
	for _, keyLen := range []int{33, 129, 257, 513, maxKeyLen} {
		payloads[fmt.Sprintf("keys of %d bytes", keyLen)] = fill(keyLen, 250<<10)
	}
	for _, tg := range decodeTargets() {
		if !tg.serverDecoded {
			continue
		}
		_ = DecodeInto(Envelope{Data: []byte(`{}`)}, reflect.New(tg.typ).Interface()) // encoding/json's cache for the type
		for name, data := range payloads {
			var err error
			limit := 4 * uint64(len(data)) // without FuzzDecode's constant, which is for what does not grow with the input
			refused := strings.HasPrefix(name, "one ")
			if refused {
				limit = uint64(tg.typ.Size()) + 64 // the value to decode into and the error
			}
			n := allocatedWithin(limit, func() {
				err = DecodeInto(Envelope{Type: tg.msgType, Data: data}, reflect.New(tg.typ).Interface())
			})
			if n > limit {
				t.Errorf("%v, %s: allocated %d bytes for %d input bytes (limit %d)", tg.typ, name, n, len(data), limit)
			}
			var fe *FieldError
			if isDataInvalid := errors.As(err, &fe) && fe.Field == "data" && fe.Reason == FieldInvalid; isDataInvalid != refused {
				t.Errorf("%v, %s: %v; want data: invalid only for a key over the limit", tg.typ, name, err)
			}
		}
	}

	// The keys inside a relay's payload have no limit, and need none: encoding/json keeps the payload as bytes, so
	// Decode allocates one copy of it, whatever its keys are. (A payload of this size is then refused for its size.)
	var payload bytes.Buffer
	payload.WriteString(`{"` + strings.Repeat("k", 100000) + `":{"a":1}`)
	for n := 0; payload.Len() < 250<<10; n++ {
		fmt.Fprintf(&payload, `,"%04d%s":[{"%s":%d}]`, n, strings.Repeat("~", 8000), strings.Repeat("K", 2049), n)
	}
	payload.WriteString(`}`)
	data := []byte(`{"toRole":"agent","kind":"a.b","payload":` + payload.String() + `}`)
	var err error
	limit := uint64(len(data))*5/4 + 1024
	if n := allocatedWithin(limit, func() { _, err = Decode[AgentSend](Envelope{Type: MessageTypeAgentSend, Data: data}) }); n > limit {
		t.Errorf("agent.send with long keys in its payload: allocated %d bytes for %d input bytes (limit %d)", n, len(data), limit)
	}
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != ErrorCodeMessageTooLarge {
		t.Errorf("agent.send with a payload of %d bytes: %v, want message_too_large", payload.Len(), err)
	}
}

// TestDecodeMalformedAllocations: a payload that is not valid JSON costs Decode its error and nothing else,
// whatever it holds. encoding/json's own error for these allocated 8 to 14 times their size.
func TestDecodeMalformedAllocations(t *testing.T) {
	for _, c := range []struct{ name, data string }{
		{"breaks off after a long key", `{"` + strings.Repeat("~", 8000) + `":`},
		{"breaks off after a long key in a list", `{"subs":[{"shareId":"s_1","` + strings.Repeat("k/", 4000) + `":`},
		{"garbage after a long key", `{"subs":[],"` + strings.Repeat("k", 20000) + `":x}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := Envelope{Type: MessageTypeSubscribeUpdate, Data: []byte(c.data)}
			var v SubscribeUpdate
			var fe *FieldError
			if err := DecodeInto(env, &v); !errors.As(err, &fe) || fe.Field != "data" || fe.Reason != FieldInvalid {
				t.Fatalf("DecodeInto: %v, want data: invalid", err)
			}
			if got := testing.AllocsPerRun(100, func() { _ = DecodeInto(env, &v) }); got > 1 {
				t.Errorf("%v allocations, want the error's own at most", got)
			}
		})
	}
}

func BenchmarkParseEnvelope(b *testing.B) {
	for _, fx := range loadFixtures(b, fixtureDir) {
		switch fx.name {
		case "pong", "room.join", "room.state", "stats.client", "pc.offer.sub":
		default:
			continue
		}
		b.Run(fx.name, func(b *testing.B) {
			b.SetBytes(int64(len(fx.raw)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := ParseEnvelope(fx.raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
