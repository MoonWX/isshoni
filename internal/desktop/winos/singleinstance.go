package winos

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Single instance (03 §13.3): one instance per Windows user session. The first instance holds a named mutex and
// serves a named pipe; a second launch finds the mutex, sends its arguments (os.Args[1:], unchanged) through the
// pipe and exits 0 within 2 s. The names are 03 §3.2's W6 fallback: the mutex Local\<id>.v1 and the pipe
// \\.\pipe\<id>.v1.<user SID>, distinct from the running mutex. The pinned Wails' own single-instance option can't
// be used: it acquires its lock inside application.New, after 03 §13.4's steps 4–6, so a second instance would
// read and delete the Pending values and take the running mutex's place in the order (README WA41's PR).
//
// The pipe accepts only the current user (its DACL) and no remote client; the first instance treats what arrives as
// untrusted arguments (app.HandleArgs).

// ForwardTimeout bounds a second instance's forward (03 §13.3, §20).
const ForwardTimeout = 2 * time.Second

// The limits of a forwarded message: a command line is short, and what arrives is untrusted.
const (
	maxForwardBytes = 64 << 10
	maxForwardArgs  = 64
	maxArgBytes     = 8 << 10
)

// ErrNotFirst is Serve's error in a process that isn't the first instance.
var ErrNotFirst = errors.New("winos: not the first instance")

// forwardMessage is what a second instance sends.
type forwardMessage struct {
	V    int      `json:"v"`
	Args []string `json:"args"`
}

func encodeArgs(args []string) ([]byte, error) {
	if args == nil {
		args = []string{}
	}
	if err := checkArgs(args); err != nil {
		return nil, err
	}
	data, err := json.Marshal(forwardMessage{V: 1, Args: args})
	if err != nil {
		return nil, err
	}
	if len(data) > maxForwardBytes {
		return nil, errors.New("winos: the arguments are too long to forward")
	}
	return data, nil
}

func decodeArgs(data []byte) ([]string, error) {
	if len(data) > maxForwardBytes {
		return nil, errors.New("winos: a forwarded message is too long")
	}
	var m forwardMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("winos: a forwarded message doesn't parse: %w", err)
	}
	if m.V != 1 || m.Args == nil {
		return nil, errors.New("winos: a forwarded message of another version")
	}
	if err := checkArgs(m.Args); err != nil {
		return nil, err
	}
	return m.Args, nil
}

func checkArgs(args []string) error {
	if len(args) > maxForwardArgs {
		return errors.New("winos: too many arguments to forward")
	}
	for _, a := range args {
		if len(a) > maxArgBytes {
			return errors.New("winos: an argument is too long to forward")
		}
	}
	return nil
}
