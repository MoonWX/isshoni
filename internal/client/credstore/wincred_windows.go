//go:build windows

package credstore

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Credential Manager (02 §11.5): generic credentials through advapi32's CredWriteW, CredReadW, CredDeleteW and
// CredEnumerateW. A LazyProc's Call reads GetLastError on the thread that made the call, so no thread is locked.

// wincred.h values.
const (
	credTypeGeneric         = 1 // CRED_TYPE_GENERIC
	credPersistLocalMachine = 2 // CRED_PERSIST_LOCAL_MACHINE: kept on this PC, never roamed
)

// credComment is the credential's Comment, shown by Control Panel's Credential Manager.
const credComment = "isshoni desktop app sign-in"

var (
	advapi32           = windows.NewLazySystemDLL("advapi32.dll")
	procCredWriteW     = advapi32.NewProc("CredWriteW")
	procCredReadW      = advapi32.NewProc("CredReadW")
	procCredDeleteW    = advapi32.NewProc("CredDeleteW")
	procCredEnumerateW = advapi32.NewProc("CredEnumerateW")
	procCredFree       = advapi32.NewProc("CredFree")
)

// credentialW is CREDENTIALW. Go lays it out as the C compiler does on every Windows architecture.
type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr // PCREDENTIAL_ATTRIBUTEW; never set
	TargetAlias        *uint16
	UserName           *uint16
}

// winStore is Open's Store on Windows.
type winStore struct {
	prefix string
	log    *slog.Logger
}

// rawCredential is a credential as Credential Manager holds it, copied out of its memory.
type rawCredential struct {
	Type       uint32
	Persist    uint32
	TargetName string
	Comment    string
	UserName   string
	Blob       []byte
}

// Open returns the Windows Credential Manager store: one generic credential per origin, target o.Prefix + origin.
// o.Dir is ignored.
func Open(o Options) (Store, error) {
	prefix, log, err := o.resolve()
	if err != nil {
		return nil, err
	}
	// A LazyProc's Call panics when the function can't be found; find them all now instead.
	for _, p := range []*windows.LazyProc{procCredWriteW, procCredReadW, procCredDeleteW, procCredEnumerateW, procCredFree} {
		if err := p.Find(); err != nil {
			return nil, fmt.Errorf("credstore: advapi32.dll: %w", err)
		}
	}
	return &winStore{prefix: prefix, log: log}, nil
}

func (s *winStore) Get(origin string) (Credential, error) {
	if err := checkOrigin(origin); err != nil {
		return Credential{}, err
	}
	raw, err := s.read(s.prefix + origin)
	if err != nil {
		return Credential{}, err
	}
	c, reason := unmarshalBlob(raw.Blob)
	clear(raw.Blob)
	if reason != "" {
		warnUnreadable(s.log, origin, reason)
		return Credential{}, ErrNotFound
	}
	c.UserName = raw.UserName
	return c, nil
}

func (s *winStore) Put(origin string, c Credential) error {
	data, err := checkPut(s.prefix, origin, c)
	if err != nil {
		return err
	}
	defer clear(data)
	return s.write(s.prefix+origin, c.UserName, data)
}

func (s *winStore) Delete(origin string) error {
	if err := checkOrigin(origin); err != nil {
		return err
	}
	target, err := utf16Ptr(s.prefix + origin)
	if err != nil {
		return err
	}
	r, _, e := procCredDeleteW.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	if r == 0 {
		if errors.Is(e, windows.ERROR_NOT_FOUND) {
			return nil
		}
		return winError("CredDeleteW", e)
	}
	return nil
}

func (s *winStore) List() ([]string, error) {
	filter, err := utf16Ptr(s.prefix + "*")
	if err != nil {
		return nil, err
	}
	var count uint32
	var list **credentialW
	r, _, e := procCredEnumerateW.Call(uintptr(unsafe.Pointer(filter)), 0,
		uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&list)))
	if r == 0 {
		if errors.Is(e, windows.ERROR_NOT_FOUND) {
			return nil, nil
		}
		return nil, winError("CredEnumerateW", e)
	}
	defer credFree(unsafe.Pointer(list))
	var origins []string
	for _, p := range unsafe.Slice(list, count) {
		if p == nil || p.Type != credTypeGeneric {
			continue
		}
		// The filter may match without regard to case; keep only this store's exact prefix.
		if origin, ok := strings.CutPrefix(windows.UTF16PtrToString(p.TargetName), s.prefix); ok && origin != "" {
			origins = append(origins, origin)
		}
	}
	slices.Sort(origins)
	return origins, nil
}

// write stores a generic credential under target with Credential Manager's CredWriteW, replacing any it had.
func (s *winStore) write(target, userName string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("%w: empty blob", errInvalid)
	}
	if len(data) > maxBlobBytes {
		return fmt.Errorf("%w: the blob is %d bytes, the limit is %d", errBlobTooLarge, len(data), maxBlobBytes)
	}
	targetW, err := utf16Ptr(target)
	if err != nil {
		return err
	}
	commentW, err := utf16Ptr(credComment)
	if err != nil {
		return err
	}
	var userW *uint16
	if userName != "" {
		if userW, err = utf16Ptr(userName); err != nil {
			return err
		}
	}
	cred := &credentialW{
		Type:               credTypeGeneric,
		TargetName:         targetW,
		Comment:            commentW,
		CredentialBlobSize: uint32(len(data)), //nolint:gosec // G115: 1 ≤ len(data) ≤ maxBlobBytes, checked above
		CredentialBlob:     &data[0],
		Persist:            credPersistLocalMachine,
		UserName:           userW,
	}
	r, _, e := procCredWriteW.Call(uintptr(unsafe.Pointer(cred)), 0)
	runtime.KeepAlive(cred)
	runtime.KeepAlive(data)
	if r == 0 {
		return winError("CredWriteW", e)
	}
	return nil
}

// read returns the generic credential named target, copied out of Credential Manager's memory, or ErrNotFound.
func (s *winStore) read(target string) (rawCredential, error) {
	targetW, err := utf16Ptr(target)
	if err != nil {
		return rawCredential{}, err
	}
	var p *credentialW
	r, _, e := procCredReadW.Call(uintptr(unsafe.Pointer(targetW)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&p)))
	if r == 0 {
		if errors.Is(e, windows.ERROR_NOT_FOUND) {
			return rawCredential{}, ErrNotFound
		}
		return rawCredential{}, winError("CredReadW", e)
	}
	defer credFree(unsafe.Pointer(p))
	raw := rawCredential{
		Type:       p.Type,
		Persist:    p.Persist,
		TargetName: windows.UTF16PtrToString(p.TargetName),
		Comment:    windows.UTF16PtrToString(p.Comment),
		UserName:   windows.UTF16PtrToString(p.UserName),
	}
	if p.CredentialBlob != nil && p.CredentialBlobSize > 0 {
		raw.Blob = slices.Clone(unsafe.Slice(p.CredentialBlob, p.CredentialBlobSize))
	}
	return raw, nil
}

// utf16Ptr converts s for a W function. checkOrigin and Options.resolve have already refused NUL in what reaches
// it, so the error is a safety net.
func utf16Ptr(s string) (*uint16, error) {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: a string contains NUL", errInvalid)
	}
	return p, nil
}

// credFree releases memory that CredReadW or CredEnumerateW allocated.
func credFree(p unsafe.Pointer) {
	_, _, _ = procCredFree.Call(uintptr(p))
}

// winError wraps a failed call's Win32 error with the function's name and the error's number, never with the
// credential: ERROR_NO_SUCH_LOGON_SESSION (no user profile loaded) becomes ErrUnavailable, every other error stays
// reachable with errors.Is.
func winError(op string, err error) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return fmt.Errorf("credstore: %s: %w", op, err)
	}
	if errors.Is(errno, windows.ERROR_NO_SUCH_LOGON_SESSION) {
		return fmt.Errorf("%w (%s: 0x%08X)", ErrUnavailable, op, uintptr(errno))
	}
	return fmt.Errorf("credstore: %s: 0x%08X: %w", op, uintptr(errno), errno)
}
