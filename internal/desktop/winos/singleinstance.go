package winos

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// Single instance (03 §13.3): one instance per Windows user on the PC, across all of that user's sessions. The first
// instance holds a named mutex and serves a named pipe; a second launch finds the mutex, sends its arguments
// (os.Args[1:], unchanged) through the pipe and exits 0 within 2 s. The names are 03 §3.2's W6 fallback
// (singleInstanceNames): the mutex Global\<id>.v1.<user SID> and the pipe \\.\pipe\<id>.v1.<user SID>, distinct
// from the running mutex. The pinned Wails' own single-instance option can't be used: it acquires its lock inside
// application.New, after 03 §13.4's steps 4–6, so a second instance would read and delete the Pending values and
// take the running mutex's place in the order (README WA41's PR).
//
// Both names have one scope, the user (README WF19). The pipe's namespace, \\.\pipe\, is shared by every session,
// and a Global\ mutex is too, so the user's SID in both names gives each user one pair, shared by the user's sessions.
// (Until WF19 the mutex was Local\, per session, and the pipe per user: a second session of the same user took the
// mutex as a first instance but couldn't create the pipe.) The scope is the user and not the session because what
// the app keeps is per user: one data root, %LOCALAPPDATA%\isshoni (06 §2.2: settings.json, servers.json, the
// WebView2 user data folder, logs, updates), and the device credentials in Credential Manager (02). Two instances of
// one user would share all of it: settings.json's last writer would win, both WebView2 environments would use one
// user data folder from two sessions, and both would refresh the same device's tokens, each from the copy it read at
// start, so the later refresh presents a spent token and the server's reuse detection revokes the device (02 §1
// decision 6, §6.2). The cost: a launch in a second session (fast user switching, RDP beside the console) forwards
// into the instance of the session that started first, whose window the second session doesn't show.
//
// A shared namespace lets any local account create the name first (the SID in it is no secret), so the name alone
// proves nothing (README §3 item 117). The mutex is created with an explicit security descriptor: owner the user's
// SID (set, not defaulted: an elevated administrator's objects are otherwise owned by BUILTIN\Administrators), and a
// DACL of the user and SYSTEM only. A launch that finds the name taken opens it for SYNCHRONIZE|READ_CONTROL only,
// rights the no-write-up label never denies, so an elevated and a non-elevated instance of one user see each other;
// the mutex is the user's only when its owner, read through that handle, is the user's SID. Anything else (another
// owner, a mutex the user may not open, another kind of object) is squatted: the launch logs a warning and falls back
// to the per-session pair, Local\<id>.v1.<SID> and \\.\pipe\<id>.v1.<SID>.session<N>, so another account can't keep
// the app from starting (one instance per session while the squat lasts; a launch after the squat ends takes the
// per-user pair beside a per-session instance still running, accepted). With that name squatted too (only a process
// in the user's own session can), the launch starts with no lock (decideLock).
//
// The pipe accepts only the current user (its DACL) and no remote client; the first instance treats what arrives as
// untrusted arguments (app.HandleArgs). The first instance creates the next pipe instance before it closes the one it
// served, so the name is never free while it serves; a second launch connects at identification level only (the
// server can't impersonate it) and forwards nothing to a server that runs as another account.

// lockNames is one pair of single-instance names: the mutex and the pipe the holder of the mutex serves.
type lockNames struct {
	mutex, pipe string
}

// singleInstanceNames derives the single-instance names of id (InstanceID; tests use their own) for the user whose
// SID is sid, in the session numbered session: the per-user pair, Global\<id>.v1.<sid> and \\.\pipe\<id>.v1.<sid>,
// both in a namespace every session shares, and the per-session fallback for a squatted per-user mutex,
// Local\<id>.v1.<sid> (the session's own namespace) and \\.\pipe\<id>.v1.<sid>.session<session>.
func singleInstanceNames(id, sid string, session uint32) (user, perSession lockNames) {
	base := id + ".v1." + sid
	sessionPipe := `\\.\pipe\` + base + ".session" + strconv.FormatUint(uint64(session), 10)
	user = lockNames{mutex: `Global\` + base, pipe: `\\.\pipe\` + base}
	return user, lockNames{mutex: `Local\` + base, pipe: sessionPipe}
}

// createOutcome is how creating a single-instance mutex's name ended (CreateMutexExW).
type createOutcome int

const (
	createdNew   createOutcome = iota // the name was free: this process created the mutex
	openedExists                      // ERROR_ALREADY_EXISTS: a handle to the existing mutex
	deniedAccess                      // ERROR_ACCESS_DENIED: a mutex of that name this user may not open
	otherObject                       // ERROR_INVALID_HANDLE: another kind of object has the name
)

// mutexState is whom a single-instance mutex's name belongs to.
type mutexState int

const (
	mutexCreated        mutexState = iota // this process created it and holds it now
	mutexOfThisUser                       // another instance of this user holds it
	mutexOfOtherAccount                   // squatted: anything that isn't provably this user's
)

// stateOf classifies a creation's outcome; ownerIsUser reports whether an existing mutex's owner, read through the
// handle, is this user's SID (false when it can't be read). Only a mutex owned by the user is the user's: the
// instances set that owner explicitly, and no other account can (03 §13.3).
func stateOf(o createOutcome, ownerIsUser bool) mutexState {
	switch {
	case o == createdNew:
		return mutexCreated
	case o == openedExists && ownerIsUser:
		return mutexOfThisUser
	default:
		return mutexOfOtherAccount
	}
}

// lockScope is the pair of names a launch ended up with.
type lockScope int

const (
	scopeUser    lockScope = iota // the per-user pair (the normal case)
	scopeSession                  // the per-session pair: another account squats the per-user mutex
	scopeNone                     // both mutexes squatted: no lock, the launch starts as a first instance
)

// decideLock is Acquire's decision from the per-user mutex's state and, read only when that one is squatted, the
// per-session mutex's: which pair the launch uses and whether it is the first instance there. A squatted name never
// makes a launch a second instance, so another account can't keep this user's app from starting.
func decideLock(user, session mutexState) (scope lockScope, first bool) {
	switch {
	case user != mutexOfOtherAccount:
		return scopeUser, user == mutexCreated
	case session != mutexOfOtherAccount:
		return scopeSession, session == mutexCreated
	default:
		return scopeNone, true
	}
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
