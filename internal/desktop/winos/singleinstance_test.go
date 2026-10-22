package winos

import (
	"strings"
	"testing"
)

func TestForwardMessage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{},
		{"--open-url", "isshoni://watch.example.com/r/lounge"},
		{"--server", "https://watch.example.com", "--autostart", "", "ünïcode"},
	} {
		data, err := encodeArgs(args)
		if err != nil {
			t.Fatalf("encode %q: %v", args, err)
		}
		got, err := decodeArgs(data)
		if err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		if len(got) != len(args) || strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Errorf("round trip %q → %q", args, got)
		}
	}
	long := strings.Repeat("x", maxArgBytes+1)
	many := make([]string, maxForwardArgs+1)
	for _, args := range [][]string{{long}, many} {
		if _, err := encodeArgs(args); err == nil {
			t.Errorf("encode of %d args (%d bytes) succeeded", len(args), len(args[0]))
		}
	}
	for _, data := range []string{
		``,
		`[]`,
		`{"v":2,"args":[]}`,
		`{"v":1}`,
		`{"v":1,"args":"x"}`,
		`{"v":1,"args":[1]}`,
		`{"v":1,"args":["` + long + `"]}`,
		strings.Repeat(" ", maxForwardBytes+1),
	} {
		if args, err := decodeArgs([]byte(data)); err == nil {
			t.Errorf("decode %.40q = %q, want an error", data, args)
		}
	}
}
