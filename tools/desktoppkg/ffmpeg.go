package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The FFmpeg build's files (06 §4.1), relative to the repository root.
var (
	pinsPath   = filepath.Join("native", "ffmpeg", "pins.json")
	lockPath   = filepath.Join("native", "ffmpeg", "lock.json")
	inputFiles = []string{"native/ffmpeg/pins.json", "native/ffmpeg/build.sh", "native/ffmpeg/ffcheck.c"}
)

// errStaleLock is the consumers' failure of 06 §4.1, word for word.
var errStaleLock = errors.New("native/ffmpeg/lock.json is stale: run the FFmpeg workflow with publish and update the lock")

// errNoLock means no publishing run has been committed yet.
var errNoLock = errors.New("native/ffmpeg/lock.json does not exist yet: run the FFmpeg workflow with publish and " +
	"commit the lock.json it prints")

// ffmpegLicense is the only license the shipped build may report (06 §4.4).
const ffmpegLicense = "LGPL version 2.1 or later"

// ffmpegEncoders are the encoders of the build, sorted (06 D6, §4.2).
var ffmpegEncoders = []string{"h264_amf", "h264_mf", "h264_nvenc", "h264_qsv"}

// forbiddenOptions must not appear in the configuration (06 §4.4).
var forbiddenOptions = []string{"--enable-gpl", "--enable-nonfree", "--enable-version3"}

// Pins is native/ffmpeg/pins.json: the build's inputs (06 §4.1).
type Pins struct {
	Revision       int `json:"revision"`
	FFmpeg         Pin `json:"ffmpeg"`
	NvCodecHeaders Pin `json:"nvCodecHeaders"`
	AMFHeaders     Pin `json:"amfHeaders"`
	Libvpl         Pin `json:"libvpl"`
}

// Pin is one upstream tarball. FFmpeg has a version, the others a tag.
type Pin struct {
	Version string `json:"version,omitempty"`
	Tag     string `json:"tag,omitempty"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// Name is the base name of the deps release's files: ffmpeg-<version>-r<revision>.
func (p *Pins) Name() string { return fmt.Sprintf("ffmpeg-%s-r%d", p.FFmpeg.Version, p.Revision) }

// Tag is the deps release's tag: deps-ffmpeg-<version>-r<revision>.
func (p *Pins) Tag() string { return "deps-" + p.Name() }

// ZipName is the build asset: ffmpeg-<version>-r<revision>-win-x64.zip.
func (p *Pins) ZipName() string { return p.Name() + "-win-x64.zip" }

// SourceName is the LGPL source asset: ffmpeg-<version>-r<revision>-source.tar.
func (p *Pins) SourceName() string { return p.Name() + "-source.tar" }

// Lock is native/ffmpeg/lock.json: what a publishing run built (06 §4.1).
type Lock struct {
	Revision     int               `json:"revision"`
	Tag          string            `json:"tag"`
	InputsSHA256 string            `json:"inputsSha256"`
	Assets       map[string]string `json:"assets"`
	AvcodecMajor int               `json:"avcodecMajor"`
	AvutilMajor  int               `json:"avutilMajor"`
	DLLs         map[string]string `json:"dlls"`
	MSVCToolset  string            `json:"msvcToolset"`
	Commit       string            `json:"commit"`
}

// AvcodecDLL is avcodec-<avcodecMajor>.dll.
func (l *Lock) AvcodecDLL() string { return "avcodec-" + strconv.Itoa(l.AvcodecMajor) + ".dll" }

// AvutilDLL is avutil-<avutilMajor>.dll.
func (l *Lock) AvutilDLL() string { return "avutil-" + strconv.Itoa(l.AvutilMajor) + ".dll" }

var (
	reSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reVersion  = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)*$`)
	reTag      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	reToolset  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	reCommit   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	pinEntries = []string{"ffmpeg", "nvCodecHeaders", "amfHeaders", "libvpl"}
)

// expectedURL is where each pinned tarball comes from; a pin bump changes only the version or tag and the SHA-256,
// so a new download location is a reviewed change of this table.
func expectedURL(entry, ref string) string {
	switch entry {
	case "ffmpeg":
		return "https://ffmpeg.org/releases/ffmpeg-" + ref + ".tar.xz"
	case "nvCodecHeaders":
		return "https://github.com/FFmpeg/nv-codec-headers/archive/refs/tags/" + ref + ".tar.gz"
	case "amfHeaders":
		return "https://github.com/GPUOpen-LibrariesAndSDKs/AMF/releases/download/" + ref + "/AMF-headers-" + ref + ".tar.gz"
	case "libvpl":
		return "https://github.com/intel/libvpl/archive/refs/tags/" + ref + ".tar.gz"
	}
	return ""
}

// parsePins reads pins.json strictly: known fields only, each URL where expectedURL says, each SHA-256 lower-case
// hex, and the layout build.sh's line-based reader needs (the revision and each entry on one line of its own).
func parsePins(data []byte) (*Pins, error) {
	var p Pins
	if err := decodeStrict(data, &p); err != nil {
		return nil, fmt.Errorf("pins.json: %w", err)
	}
	var problems []string
	if p.Revision < 1 {
		problems = append(problems, "revision must be 1 or more")
	}
	for _, name := range pinEntries {
		pin := p.entry(name)
		ref, kind := pin.Tag, "tag"
		if name == "ffmpeg" {
			ref, kind = pin.Version, "version"
			if pin.Tag != "" {
				problems = append(problems, "ffmpeg has a tag; it takes a version")
			}
			if !reVersion.MatchString(ref) {
				problems = append(problems, fmt.Sprintf("ffmpeg.version %q is not a version such as 9.0.2", ref))
			}
		} else {
			if pin.Version != "" {
				problems = append(problems, name+" has a version; it takes a tag")
			}
			if !reTag.MatchString(ref) {
				problems = append(problems, fmt.Sprintf("%s.tag %q is not a tag", name, ref))
			}
		}
		if want := expectedURL(name, ref); pin.URL != want {
			problems = append(problems, fmt.Sprintf("%s.url is %q; for %s %q it is %q", name, pin.URL, kind, ref, want))
		}
		if !reSHA256.MatchString(pin.SHA256) {
			problems = append(problems, fmt.Sprintf("%s.sha256 %q is not a lower-case hex SHA-256", name, pin.SHA256))
		}
	}
	problems = append(problems, pinsLayoutProblems(data)...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("pins.json: %s", strings.Join(problems, "; "))
	}
	return &p, nil
}

func (p *Pins) entry(name string) Pin {
	switch name {
	case "ffmpeg":
		return p.FFmpeg
	case "nvCodecHeaders":
		return p.NvCodecHeaders
	case "amfHeaders":
		return p.AMFHeaders
	default:
		return p.Libvpl
	}
}

// pinsLayoutProblems checks the layout native/ffmpeg/build.sh reads without a JSON parser: "revision" and each entry
// on exactly one line, the entry's whole object on that line.
func pinsLayoutProblems(data []byte) []string {
	var problems []string
	lines := strings.Split(string(data), "\n")
	count := func(re *regexp.Regexp) int {
		n := 0
		for _, l := range lines {
			if re.MatchString(l) {
				n++
			}
		}
		return n
	}
	if count(regexp.MustCompile(`^\s*"revision":\s*[0-9]+\s*,?\s*$`)) != 1 {
		problems = append(problems, `"revision" must be on a line of its own (build.sh reads pins.json line by line)`)
	}
	for _, name := range pinEntries {
		whole := regexp.MustCompile(`^\s*"` + name + `":\s*\{[^{}]*\}\s*,?\s*$`)
		if count(whole) != 1 {
			problems = append(problems, fmt.Sprintf("%q must be one line holding its whole object (build.sh reads pins.json line by line)", name))
		}
	}
	return problems
}

// parseLock reads lock.json strictly and checks its fields' forms.
func parseLock(data []byte) (*Lock, error) {
	var l Lock
	if err := decodeStrict(data, &l); err != nil {
		return nil, fmt.Errorf("lock.json: %w", err)
	}
	var problems []string
	if l.Revision < 1 {
		problems = append(problems, "revision must be 1 or more")
	}
	if !strings.HasPrefix(l.Tag, "deps-ffmpeg-") {
		problems = append(problems, fmt.Sprintf("tag %q is not deps-ffmpeg-<version>-r<revision>", l.Tag))
	}
	if !reSHA256.MatchString(l.InputsSHA256) {
		problems = append(problems, "inputsSha256 is not a lower-case hex SHA-256")
	}
	name := strings.TrimPrefix(l.Tag, "deps-")
	if len(l.Assets) != 2 || !reSHA256.MatchString(l.Assets[name+"-win-x64.zip"]) ||
		!reSHA256.MatchString(l.Assets[name+"-source.tar"]) {
		problems = append(problems, fmt.Sprintf("assets must be exactly %s-win-x64.zip and %s-source.tar with their SHA-256s", name, name))
	}
	if l.AvcodecMajor < 1 || l.AvutilMajor < 1 {
		problems = append(problems, "avcodecMajor and avutilMajor must be positive integers")
	}
	if len(l.DLLs) != 2 || !reSHA256.MatchString(l.DLLs[l.AvcodecDLL()]) || !reSHA256.MatchString(l.DLLs[l.AvutilDLL()]) {
		problems = append(problems, fmt.Sprintf("dlls must be exactly %s and %s with their SHA-256s", l.AvcodecDLL(), l.AvutilDLL()))
	}
	if !reToolset.MatchString(l.MSVCToolset) {
		problems = append(problems, fmt.Sprintf("msvcToolset %q is not a toolset build such as 14.44.35207", l.MSVCToolset))
	}
	if !reCommit.MatchString(l.Commit) {
		problems = append(problems, "commit is not a full commit hash")
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("lock.json: %s", strings.Join(problems, "; "))
	}
	return &l, nil
}

// decodeStrict decodes one JSON value with no unknown fields and nothing after it.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the JSON value")
	}
	return nil
}

// inputsSHA256 is lock.json's inputsSha256: the SHA-256 of the lines "<sha256>  <path>\n" for pins.json, build.sh
// and ffcheck.c, in that order (06 §4.1; native/ffmpeg/build.sh computes the same).
func inputsSHA256(root string) (string, error) {
	var lines bytes.Buffer
	for _, rel := range inputFiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&lines, "%s  %s\n", hex.EncodeToString(sum[:]), rel)
	}
	sum := sha256.Sum256(lines.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// ffmpegState is the repository's FFmpeg pins, inputs hash and lock.
type ffmpegState struct {
	pins   *Pins
	inputs string
	lock   *Lock // nil when lock.json does not exist
}

// readFFmpegState reads and checks pins.json, computes inputsSha256 and reads lock.json when it exists.
func readFFmpegState(root string) (*ffmpegState, error) {
	data, err := os.ReadFile(filepath.Join(root, pinsPath))
	if err != nil {
		return nil, err
	}
	pins, err := parsePins(data)
	if err != nil {
		return nil, err
	}
	inputs, err := inputsSHA256(root)
	if err != nil {
		return nil, err
	}
	st := &ffmpegState{pins: pins, inputs: inputs}
	data, err = os.ReadFile(filepath.Join(root, lockPath))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, nil
	case err != nil:
		return nil, err
	}
	if st.lock, err = parseLock(data); err != nil {
		return nil, err
	}
	return st, nil
}

// currentLock returns the lock when it describes the current inputs: errNoLock without one, errStaleLock when the
// inputs changed since, and an error when its tag and assets don't follow from pins.json.
func (st *ffmpegState) currentLock() (*Lock, error) {
	if st.lock == nil {
		return nil, errNoLock
	}
	l := st.lock
	if l.InputsSHA256 != st.inputs {
		return nil, errStaleLock
	}
	var problems []string
	if l.Revision != st.pins.Revision {
		problems = append(problems, fmt.Sprintf("revision %d, pins.json says %d", l.Revision, st.pins.Revision))
	}
	if l.Tag != st.pins.Tag() {
		problems = append(problems, fmt.Sprintf("tag %q, pins.json gives %q", l.Tag, st.pins.Tag()))
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("lock.json does not match pins.json: %s", strings.Join(problems, "; "))
	}
	return l, nil
}

// FFmpegConfig is the part of ffcheck's ffmpeg-config.json that the checks read (06 §4.4).
type FFmpegConfig struct {
	AvcodecMajor  int      `json:"avcodecMajor"`
	AvutilMajor   int      `json:"avutilMajor"`
	License       string   `json:"license"`
	AvutilLicense string   `json:"avutilLicense"`
	Configuration string   `json:"configuration"`
	Encoders      []string `json:"encoders"`
	Decoders      []string `json:"decoders"`
	OK            bool     `json:"ok"`
}

// checkFFmpegConfig applies 06 §4.4's LGPL checks to an ffmpeg-config.json: the license, no gpl, nonfree or
// version3 option, exactly the four encoders and no decoder.
func checkFFmpegConfig(data []byte) (*FFmpegConfig, error) {
	var c FFmpegConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("ffmpeg-config.json: %w", err)
	}
	var problems []string
	if c.License != ffmpegLicense {
		problems = append(problems, fmt.Sprintf("license is %q, not %q", c.License, ffmpegLicense))
	}
	if c.AvutilLicense != ffmpegLicense {
		problems = append(problems, fmt.Sprintf("avutilLicense is %q, not %q", c.AvutilLicense, ffmpegLicense))
	}
	if c.Configuration == "" {
		problems = append(problems, "the configuration is empty")
	}
	for _, opt := range forbiddenOptions {
		if strings.Contains(c.Configuration, opt) {
			problems = append(problems, "the configuration contains "+opt)
		}
	}
	got := slices.Sorted(slices.Values(c.Encoders))
	if !slices.Equal(got, ffmpegEncoders) {
		problems = append(problems, fmt.Sprintf("the encoders are %v, not exactly %v", got, ffmpegEncoders))
	}
	if len(c.Decoders) > 0 {
		problems = append(problems, fmt.Sprintf("the build has decoders: %v", c.Decoders))
	}
	if !c.OK {
		problems = append(problems, `ffcheck's verdict "ok" is not true`)
	}
	if c.AvcodecMajor < 1 || c.AvutilMajor < 1 {
		problems = append(problems, "avcodecMajor or avutilMajor is missing")
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("ffmpeg-config.json: %s", strings.Join(problems, "; "))
	}
	return &c, nil
}

// runFFmpegInputs is `desktoppkg ffmpeg-inputs [-check]`.
func runFFmpegInputs(_ context.Context, e *env, args []string) error {
	fs := flag.NewFlagSet("ffmpeg-inputs", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	check := fs.Bool("check", false, "fail unless native/ffmpeg/lock.json describes the current inputs")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: ffmpeg-inputs takes no arguments", errUsage)
	}
	st, err := readFFmpegState(e.root)
	if err != nil {
		return err
	}
	if *check {
		if _, err := st.currentLock(); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(e.stdout, st.inputs)
	return err
}
