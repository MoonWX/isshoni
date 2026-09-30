package protocol_test

import (
	"errors"
	"fmt"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// A server-side view of one request: parse the frame, check the role, decode and validate the payload, and answer
// with ok or error.
func Example() {
	frame := []byte(`{"type":"subscribe.update","id":"14","data":{"subs":[{"shareId":"s_q7m2","video":"high","audio":"on"},` +
		`{"shareId":"s_q7m2","video":"low","audio":"off"}]}}`)

	env, err := protocol.ParseEnvelope(frame)
	if err != nil {
		return // bad_message: close with 4400
	}
	spec, ok := protocol.Lookup(env.Type, protocol.DirClientToServer)
	if !ok || !protocol.Allowed(protocol.RoleViewer, env.Type, "") {
		return // unknown_type or forbidden
	}
	var reply []byte
	if _, err := protocol.Decode[protocol.SubscribeUpdate](env); err != nil {
		var fe *protocol.FieldError
		if errors.As(err, &fe) {
			reply, _ = protocol.Marshal(protocol.MessageTypeError, "", env.ID, fe.BadRequest(protocol.ErrorScopeRequest))
		}
	} else {
		reply, _ = protocol.Marshal(spec.Reply, "", env.ID, protocol.SubscribeResult{})
	}
	fmt.Println(string(reply))
	// Output: {"type":"error","re":"14","data":{"code":"bad_request","retryable":false,"scope":"request","params":{"field":"subs[1].shareId","reason":"duplicate"}}}
}

func ExampleParseH264CodecKey() {
	fmt.Println(protocol.ParseH264CodecKey("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640034"))
	fmt.Println(protocol.ParseH264CodecKey("packetization-mode=0;profile-level-id=42e01f") == "")
	// Output:
	// h264/6400
	// true
}

func ExampleSecret() {
	token := protocol.Secret("r1.do-not-log-me")
	fmt.Printf("%v %s %q\n", token, token, token)
	fmt.Println(token.Reveal() == "r1.do-not-log-me")
	// Output:
	// [redacted] [redacted] [redacted]
	// true
}
