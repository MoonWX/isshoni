package winos

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Single instance (03 §13.3; README §3 item 119): one instance per Windows user and data root on the PC, across all
// of that user's sessions, elevated or not. The pinned Wails' own single-instance option can't be used: it acquires its
// lock inside application.New, after 03 §13.4's steps 4–6, so a second instance would read and delete the Pending
// values and take the running mutex's place in the order (README WA41's PR).
//
// The lock is a file lock in the data root, %LOCALAPPDATA%\isshoni (per user and per PC; the e2e build's
// ISSHONI_E2E_DATA_DIR replaces the folder above it, so each test data root has its own lock): the first instance holds
// LockFileEx on a byte range of instance.lock far past its content (lockOffset), which no other process can take until
// it closes the file or exits. The folder is the user's own (only the user, SYSTEM and administrators may open it), so
// no name in a namespace that other accounts share decides who is first, and nothing needs to stay unknown: another
// account can neither take the lock nor put a file or a link at its name. The scope is the user and not the session
// because what the app keeps is per user: one data root (06 §2.2: settings.json, servers.json, the WebView2 user data
// folder, logs, updates) and the device credentials in Credential Manager (02); two instances of one user would share
// all of it (03 §13.3 says how that ends). The cost: a launch in a second session forwards into the instance of the
// session that started first.
//
// The pipe: the first instance creates \\.\pipe\<id>.v1.<16 random bytes in hex>, a new name each run, with
// FILE_FLAG_FIRST_PIPE_INSTANCE, owned by the user and open to the user only, and then writes its name into
// instance.lock. A second launch reads the name there, opens the pipe and forwards only when the pipe's owner is the
// user's SID (serverCheck), so a pipe that another account created at a name it learnt (every pipe's name is listable)
// gets nothing. The first instance treats what arrives as untrusted arguments (app.HandleArgs); it creates the next
// pipe instance before it closes the one it served, so the name is never free while it serves, and a second launch
// connects at identification level only (the server can't impersonate it). Neither the pipe's name nor the file's
// content is ever logged or put in an error: not secret, but nobody's business.

// pipeTokenBytes is the random part of the pipe's name.
const pipeTokenBytes = 16

// pipePrefix is the pipe's name of id (InstanceID; tests use their own) up to its random part. The
// "io.isshoni.desktop." in it is what the workflows' cleanup looks for (ciPipePattern in the tests).
func pipePrefix(id string) string { return `\\.\pipe\` + id + ".v1." }

// newPipeName returns a pipe name of id with a new random part.
func newPipeName(id string) (string, error) {
	b := make([]byte, pipeTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("winos: a random pipe name: %w", err)
	}
	return pipePrefix(id) + hex.EncodeToString(b), nil
}

// pipeFromLock returns the pipe name that instance.lock's content data holds for id: the prefix and 32 lowercase hex
// digits, surrounded by white space at most. Anything else (an empty file while the first instance starts, a
// half-written one, another id's name, garbage) gives ok false; the content is never in a log or an error.
func pipeFromLock(id string, data []byte) (pipe string, ok bool) {
	s := strings.TrimSpace(string(data))
	token, found := strings.CutPrefix(s, pipePrefix(id))
	if !found || len(token) != 2*pipeTokenBytes {
		return "", false
	}
	for _, c := range token {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return s, true
}

// errLockNotOurs: instance.lock isn't a plain file owned by this user. Only another account with rights over the
// user's profile (an administrator) or a bug puts such a thing there; the launch then starts without the lock.
var errLockNotOurs = errors.New("winos: the single-instance lock file isn't this user's own plain file; " +
	"starting without the lock")

// lockFacts is what Acquire found at instance.lock.
type lockFacts struct {
	reparse     bool  // a reparse point (a symbolic link or junction), opened as itself
	ownerIsUser bool  // its owner, read through the handle, is this user's SID (false when unread)
	lockErr     error // LockFileEx's error; nil: this process holds the lock
	violation   bool  // lockErr is ERROR_LOCK_VIOLATION: another handle holds the range
}

// lockState is Acquire's decision (03 §13.3): only a plain file this user owns is the lock. On it, the held range
// makes this the first instance, and a range another handle holds (any process of the user and data root, in any
// session, elevated or not) a second instance. Anything else is an error, so the caller starts as the first instance
// without the lock and never exits as a second one because of a file it can't trust.
func lockState(f lockFacts) (first bool, err error) {
	switch {
	case f.reparse || !f.ownerIsUser:
		return false, errLockNotOurs
	case f.lockErr == nil:
		return true, nil
	case f.violation:
		return false, nil
	default:
		return false, fmt.Errorf("winos: the single-instance lock: %w", f.lockErr)
	}
}

// errServerNotUser: the pipe that answered isn't provably this user's; nothing was forwarded.
var errServerNotUser = errors.New("winos: the pipe isn't served by this user; nothing was forwarded")

// serverCheck is Forward's decision on the pipe's server (03 §13.3). owner is the pipe's owner SID, read through the
// client's handle (ownerErr when it can't be read); the first instance sets it to the user's SID, which no other
// account can, and reading it works whether or not the server runs elevated. Only when the owner can't be read is the
// server's process token compared (token: its user's SID); a token that can't be opened (ERROR_ACCESS_DENIED for an
// elevated server and a non-elevated client) leaves the server unproven.
func serverCheck(sid, owner string, ownerErr error, token func() (string, error)) error {
	if ownerErr == nil {
		if owner == sid {
			return nil
		}
		return errServerNotUser
	}
	tokenSID, err := token()
	if err != nil {
		return fmt.Errorf("%w (its owner: %w; its token: %w)", errServerNotUser, ownerErr, err)
	}
	if tokenSID != sid {
		return errServerNotUser
	}
	return nil
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
	// one gets a new random name that nobody else holds, which is then published in instance.lock; later ones take
	// that name. It returns errStopped when Close ended its retries.
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
