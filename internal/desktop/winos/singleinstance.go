package winos

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// Single instance (03 §13.3): one instance per Windows user session. The first instance holds a named mutex and
// serves a named pipe; a second launch finds the mutex, sends its arguments (os.Args[1:], unchanged) through the
// pipe and exits 0 within 2 s. The names are 03 §3.2's W6 fallback (singleInstanceNames): the mutex Local\<id>.v1
// and the pipe \\.\pipe\<id>.v1.<user SID>.<session id>, distinct from the running mutex. The pinned Wails' own
// single-instance option can't be used: it acquires its lock inside application.New, after 03 §13.4's steps 4–6, so
// a second instance would read and delete the Pending values and take the running mutex's place in the order (README
// WA41's PR).
//
// Both names have one scope, the user session. A Local\ object lives in its session's own namespace, but all sessions
// share \\.\pipe\: with only the user's SID in the pipe's name, a second session of the same user (fast user
// switching to the same account, RDP beside the console, a leftover of another session) took the mutex as a first
// instance yet couldn't create the pipe, which the other session's instance held, so its second launches forwarded
// into that session's window or failed. The session id in the name gives each session its own pipe. The scope is
// the session, not the user (a Global\ mutex), because the rest of 03 §13.4's contract is per session: the running
// mutex and the installer's mutex are Local\, so an installer waits for the app of its own session, and a link
// clicked in one session must open in a window that session shows.
//
// The pipe accepts only the current user (its DACL) and no remote client; the first instance treats what arrives as
// untrusted arguments (app.HandleArgs). The first instance creates the next pipe instance before it closes the one it
// served, so the name is never free while it serves; a second launch connects at identification level only (the
// server can't impersonate it) and forwards nothing to a server that runs as another account.

// singleInstanceNames derives the single-instance names of id (InstanceID; tests use their own) for the user whose
// SID is sid, in the session of id session (ProcessIdToSessionId): the mutex Local\<id>.v1 and the pipe
// \\.\pipe\<id>.v1.<sid>.<session>, the session in decimal.
func singleInstanceNames(id, sid string, session uint32) (mutex, pipe string) {
	return `Local\` + id + ".v1", `\\.\pipe\` + id + ".v1." + sid + "." + strconv.FormatUint(uint64(session), 10)
}

// ForwardTimeout bounds a second instance's forward (03 §13.3, §20).
const ForwardTimeout = 2 * time.Second

// The limits of a forwarded message: a command line is short, and what arrives is untrusted.
const (
	maxForwardBytes = 64 << 10
	maxForwardArgs  = 64
	maxArgBytes     = 8 << 10
)

// forwardArrivedMsg is serveLoop's log line for each second launch's command line it delivers (the count only);
// cmd/isshoni-desktop's TestAutostartRoundTrip looks for it.
const forwardArrivedMsg = "winos: a second launch's command line arrived"

// ErrNotFirst is Serve's error in a process that isn't the first instance.
var ErrNotFirst = errors.New("winos: not the first instance")

// errStopped: Close ran while the pipe's server waited.
var errStopped = errors.New("winos: stopped")

// pipeServer is the OS's side of Serve's goroutine: instances of the pipe's name (singleinstance_windows.go). The loop
// around it, serveLoop, is portable, so its order is tested on every OS (singleinstance_test.go).
type pipeServer interface {
	// create makes an instance of the pipe that a client may connect to at once, before accept is called. The first
	// one is made with nobody else holding the name. It returns errStopped when Close ended its retries.
	create(first bool) (pipeInstance, error)
	// stopped reports whether Close ran.
	stopped() bool
}

// pipeInstance is one instance of the pipe.
type pipeInstance interface {
	// accept waits for this instance's client (one that connected before the call counts, even one that wrote and
	// closed its end since) and returns all it wrote until it closed its end. It returns errStopped on Close.
	accept() ([]byte, error)
	// close disconnects the instance's client, if any, and closes the instance.
	close()
}

// serveLoop is Serve's goroutine: it delivers each second launch's command line to onArgs, one at a time, until Close.
// The next instance is created before the served one is closed, so the name is never free for another account to
// create while this process serves, and a launch that comes while onArgs runs connects to that instance and is served
// once onArgs returns.
func serveLoop(p pipeServer, onArgs func(args []string), log *slog.Logger) {
	// The first instance of the pipe must be ours (nobody squats the name).
	inst, err := p.create(true)
	if err != nil {
		if !errors.Is(err, errStopped) {
			log.Warn("winos: the single-instance pipe can't be created; second launches can't reach this one",
				"err", err)
		}
		return
	}
	for {
		if p.stopped() { // Close ran while onArgs handled the last command line
			inst.close()
			return
		}
		data, err := inst.accept()
		quit := errors.Is(err, errStopped)
		var next pipeInstance
		var nextErr error
		if !quit {
			next, nextErr = p.create(false)
		}
		inst.close()
		if quit {
			return
		}
		var args []string
		if err == nil {
			args, err = decodeArgs(data)
		}
		if err != nil {
			log.Info("winos: a second launch's message was dropped", "err", err)
		} else {
			// Its count only: what arrives may hold a link. The first instance's own record that a forward came, so a
			// second launch whose window never showed reads apart from one whose forward never arrived.
			log.Info(forwardArrivedMsg, "args", len(args))
			onArgs(args)
		}
		if nextErr != nil {
			if !errors.Is(nextErr, errStopped) {
				log.Warn("winos: the single-instance pipe can't be created; second launches can't reach this one",
					"err", nextErr)
			}
			return
		}
		inst = next
	}
}

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
