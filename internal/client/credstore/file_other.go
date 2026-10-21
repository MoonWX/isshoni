//go:build !windows

package credstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// The file store (02 §11.5, "Other OSes in M2"): every credential of every prefix in one JSON document,
//
//	{"version": 1, "credentials": {"isshoni:v1:https://watch.example.com": {"v": 1, "refreshToken": …,
//	  "deviceId": …, "savedAt": …, "userName": …}}}
//
// keyed by prefix + origin like Credential Manager's target names, so stores with different prefixes share a
// directory without seeing each other. Every operation reads the file again, so stores on one file in one process
// agree; writes replace the file atomically (a temporary file in the same directory, synced, then renamed) with mode
// 0600 in a directory created 0700.
const (
	credentialsFile    = "credentials.json"
	credentialsVersion = 1
	devDirName         = "isshoni-dev" // under os.UserConfigDir()
)

// credentialsDoc is credentials.json.
type credentialsDoc struct {
	Version     int                        `json:"version"`
	Credentials map[string]json.RawMessage `json:"credentials"`
}

// errCorruptFile is load's answer for a file that doesn't parse or has another version.
var errCorruptFile = errors.New("credstore: the credentials file can't be read")

// fileLocks holds one mutex per credentials file (by absolute path), shared by every fileStore of the process.
var fileLocks sync.Map

// fileStore is Open's Store outside Windows.
type fileStore struct {
	path   string // absolute path of credentials.json
	prefix string
	log    *slog.Logger
	mu     *sync.Mutex // fileLocks' entry for path

	// Seams for the atomic-write tests; nil means the real thing.
	writeTemp func(f *os.File, data []byte) error // writes and syncs the temporary file
	rename    func(oldpath, newpath string) error
}

// Open returns the plaintext file store (credentials.json in o.Dir) and logs one WARN saying it is for development
// only. Windows builds get Credential Manager instead (wincred_windows.go).
func Open(o Options) (Store, error) {
	prefix, log, err := o.resolve()
	if err != nil {
		return nil, err
	}
	dir := o.Dir
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("credstore: no directory for the credentials file: %w", err)
		}
		dir = filepath.Join(base, devDirName)
	}
	path, err := filepath.Abs(filepath.Join(dir, credentialsFile))
	if err != nil {
		return nil, fmt.Errorf("credstore: the credentials file's path: %w", err)
	}
	mu, _ := fileLocks.LoadOrStore(path, new(sync.Mutex))
	// The path is left out: it names the OS account, and the app's logs may be shared for support.
	log.Warn("credstore: plaintext credential store, development only")
	return &fileStore{path: path, prefix: prefix, log: log, mu: mu.(*sync.Mutex)}, nil
}

func (s *fileStore) Get(origin string) (Credential, error) {
	if err := checkOrigin(origin); err != nil {
		return Credential{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if errors.Is(err, errCorruptFile) {
		s.warnCorrupt()
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, err
	}
	raw, ok := doc.Credentials[s.prefix+origin]
	if !ok {
		return Credential{}, ErrNotFound
	}
	c, reason := unmarshalBlob(raw)
	if reason != "" {
		warnUnreadable(s.log, origin, reason)
		return Credential{}, ErrNotFound
	}
	return c, nil
}

func (s *fileStore) Put(origin string, c Credential) error {
	if _, err := checkPut(s.prefix, origin, c); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if errors.Is(err, errCorruptFile) {
		// Set the unreadable file aside rather than overwrite it, as servers.json does (02 §11.6).
		if err := s.renameFile(s.path, s.path+".bad"); err != nil {
			return fmt.Errorf("credstore: set the unreadable credentials file aside: %w", err)
		}
		s.log.Warn("credstore: the credentials file can't be read; renamed it to credentials.json.bad and started a new one")
		doc, err = emptyDoc(), nil
	}
	if err != nil {
		return err
	}
	doc.Credentials[s.prefix+origin] = marshalBlob(c, true)
	return s.save(doc)
}

func (s *fileStore) Delete(origin string) error {
	if err := checkOrigin(origin); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if errors.Is(err, errCorruptFile) {
		s.warnCorrupt() // nothing readable to delete; the next Put sets the file aside
		return nil
	}
	if err != nil {
		return err
	}
	key := s.prefix + origin
	if _, ok := doc.Credentials[key]; !ok {
		return nil
	}
	delete(doc.Credentials, key)
	return s.save(doc)
}

func (s *fileStore) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if errors.Is(err, errCorruptFile) {
		s.warnCorrupt()
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var origins []string
	for _, key := range slices.Sorted(maps.Keys(doc.Credentials)) {
		if origin, ok := strings.CutPrefix(key, s.prefix); ok && origin != "" {
			origins = append(origins, origin)
		}
	}
	return origins, nil
}

func emptyDoc() credentialsDoc {
	return credentialsDoc{Version: credentialsVersion, Credentials: make(map[string]json.RawMessage)}
}

// load reads credentials.json: no file is an empty document, one that doesn't parse or has another version is
// errCorruptFile.
func (s *fileStore) load() (credentialsDoc, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return emptyDoc(), nil
	}
	if err != nil {
		return credentialsDoc{}, fmt.Errorf("credstore: read the credentials file: %w", err)
	}
	var doc credentialsDoc
	if err := json.Unmarshal(data, &doc); err != nil || doc.Version != credentialsVersion {
		return credentialsDoc{}, errCorruptFile
	}
	if doc.Credentials == nil {
		doc.Credentials = make(map[string]json.RawMessage)
	}
	return doc, nil
}

func (s *fileStore) warnCorrupt() {
	s.log.Warn("credstore: the credentials file can't be read; treating it as empty")
}

// save replaces credentials.json with doc atomically: a temporary file in the same directory (mode 0600), written
// and synced, then renamed over the old one. On any failure the old file is left as it was and the temporary file
// is removed.
func (s *fileStore) save(doc credentialsDoc) (err error) {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("credstore: encode the credentials file: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("credstore: create the credentials directory: %w", err)
	}
	f, err := os.CreateTemp(dir, "."+credentialsFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("credstore: create a temporary credentials file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("credstore: set the temporary credentials file's mode: %w", err)
	}
	write := s.writeTemp
	if write == nil {
		write = writeAndSync
	}
	if err := write(f, data); err != nil {
		return fmt.Errorf("credstore: write the temporary credentials file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("credstore: close the temporary credentials file: %w", err)
	}
	if err := s.renameFile(tmp, s.path); err != nil {
		return fmt.Errorf("credstore: replace the credentials file: %w", err)
	}
	syncDir(dir)
	return nil
}

func (s *fileStore) renameFile(oldpath, newpath string) error {
	if s.rename != nil {
		return s.rename(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// syncDir makes a rename in dir durable where the OS allows it; a failure only weakens durability, not atomicity.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
