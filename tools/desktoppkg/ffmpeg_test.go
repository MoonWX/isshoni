package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the repository this package lives in (tools/desktoppkg → ../..).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestCommittedPins checks native/ffmpeg/pins.json: its schema (known fields, the URL each entry must come from,
// lower-case SHA-256s) and the one-line-per-entry layout that native/ffmpeg/build.sh reads without a JSON parser.
// The versions it pins are not checked here, so a security bump (06 §4.7) changes only pins.json and lock.json;
// TestCommittedLock checks that a current lock names the same revision and tag.
func TestCommittedPins(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), pinsPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsePins(data); err != nil {
		t.Fatal(err)
	}
}

// TestCommittedLock: native/ffmpeg/lock.json, once a publishing run has been committed, is well formed and either
// describes the current inputs or is reported as stale; before that it is absent.
func TestCommittedLock(t *testing.T) {
	st, err := readFFmpegState(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.lock == nil {
		if _, err := st.currentLock(); !errors.Is(err, errNoLock) {
			t.Errorf("without lock.json: %v, want errNoLock", err)
		}
		t.Skip("native/ffmpeg/lock.json is not committed yet (the owner's publishing run, README §8)")
	}
	if _, err := st.currentLock(); err != nil && !errors.Is(err, errStaleLock) {
		t.Error(err)
	}
}

const goodPins = `{
  "revision": 1,
  "ffmpeg":         {"version": "9.0.2", "url": "https://ffmpeg.org/releases/ffmpeg-9.0.2.tar.xz", "sha256": "8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e"},
  "nvCodecHeaders": {"tag": "n12.1.14.1", "url": "https://github.com/FFmpeg/nv-codec-headers/archive/refs/tags/n12.1.14.1.tar.gz", "sha256": "f40c8a27417eb2688412da32985e382e811196311c4d103d1fcaafad2876cfc1"},
  "amfHeaders":     {"tag": "v1.5.3", "url": "https://github.com/GPUOpen-LibrariesAndSDKs/AMF/releases/download/v1.5.3/AMF-headers-v1.5.3.tar.gz", "sha256": "0f521ad98041f8001147cded4a660127f18b1c27a801a5cd691bda1fa48381be"},
  "libvpl":         {"tag": "v2.17.0", "url": "https://github.com/intel/libvpl/archive/refs/tags/v2.17.0.tar.gz", "sha256": "4de3e2faf1e8307fb282e4a43f443191810f6a6b0a484fffa7995ba1c814c6ec"}
}
`

func TestParsePins(t *testing.T) {
	p, err := parsePins([]byte(goodPins))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "ffmpeg-9.0.2-r1" || p.ZipName() != "ffmpeg-9.0.2-r1-win-x64.zip" ||
		p.SourceName() != "ffmpeg-9.0.2-r1-source.tar" || p.Tag() != "deps-ffmpeg-9.0.2-r1" {
		t.Errorf("names: %s %s %s %s", p.Name(), p.ZipName(), p.SourceName(), p.Tag())
	}

	for _, tc := range []struct {
		name, old, new, want string
	}{
		{"revision 0", `"revision": 1`, `"revision": 0`, "revision must be 1 or more"},
		{"unknown field", `"revision": 1,`, `"revision": 1, "extra": 2,`, "unknown field"},
		{"upper-case hash", "8c3850283eb25fa0", "8C3850283EB25FA0", "ffmpeg.sha256"},
		{"short hash", `"sha256": "f40c8a27417eb2688412da32985e382e811196311c4d103d1fcaafad2876cfc1"`, `"sha256": "f40c"`, "nvCodecHeaders.sha256"},
		{"other host", "https://ffmpeg.org/releases/", "https://mirror.example/releases/", "ffmpeg.url"},
		{"http", "https://github.com/intel", "http://github.com/intel", "libvpl.url"},
		{"url of another version", "AMF-headers-v1.5.3.tar.gz", "AMF-headers-v1.5.2.tar.gz", "amfHeaders.url"},
		{"version not a version", `"version": "9.0.2", "url": "https://ffmpeg.org/releases/ffmpeg-9.0.2.tar.xz"`, `"version": "latest", "url": "https://ffmpeg.org/releases/ffmpeg-latest.tar.xz"`, "ffmpeg.version"},
		{"tag for ffmpeg", `{"version": "9.0.2",`, `{"version": "9.0.2", "tag": "n9.0.2",`, "ffmpeg has a tag"},
		{"version for libvpl", `{"tag": "v2.17.0",`, `{"tag": "v2.17.0", "version": "2.17.0",`, "libvpl has a version"},
		{"missing entry", `,
  "libvpl":         {"tag": "v2.17.0", "url": "https://github.com/intel/libvpl/archive/refs/tags/v2.17.0.tar.gz", "sha256": "4de3e2faf1e8307fb282e4a43f443191810f6a6b0a484fffa7995ba1c814c6ec"}`, "", "libvpl.url"},
		{"entry over two lines", `"amfHeaders":     {"tag": "v1.5.3",`, "\"amfHeaders\":     {\"tag\": \"v1.5.3\",\n   ", `"amfHeaders" must be one line`},
		{"revision with an entry", "\"revision\": 1,\n  \"ffmpeg\":", "\"revision\": 1, \"ffmpeg\":", `"revision" must be on a line of its own`},
		{"trailing data", "c6ec\"}\n}\n", "c6ec\"}\n}\n{}\n", "data after the JSON value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := strings.Replace(goodPins, tc.old, tc.new, 1)
			if data == goodPins {
				t.Fatalf("the replacement of %q changed nothing", tc.old)
			}
			if _, err := parsePins([]byte(data)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestInputsSHA256 pins the format of lock.json's inputsSha256 (06 §4.1) with a value computed by the shell:
// for f in pins.json build.sh ffcheck.c; do printf '%s  native/ffmpeg/%s\n' "$(shasum -a 256 …/$f | cut -c1-64)" "$f"; done | shasum -a 256
func TestInputsSHA256(t *testing.T) {
	got, err := inputsSHA256(filepath.Join("testdata", "inputs"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "d8e833b4d95f1245236aa7bfc92db5920d013a0051846130be239937d03951b1"; got != want {
		t.Errorf("inputsSHA256 = %s, want %s", got, want)
	}
	if _, err := inputsSHA256(t.TempDir()); err == nil {
		t.Error("no error without the files")
	}
}

// TestInputsSHA256MatchesBuildScript: native/ffmpeg/build.sh's inputs_sha256 (what the build records) and this
// command (what consumers compare with lock.json) give the same hash for the committed files.
func TestInputsSHA256MatchesBuildScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum")
	}
	root := repoRoot(t)
	cmd := exec.CommandContext(t.Context(), bash, "-c", `. native/ffmpeg/build.sh && inputs_sha256`)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("build.sh's inputs_sha256: %v", err)
	}
	want, err := inputsSHA256(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("build.sh says %s, desktoppkg %s", got, want)
	}
}

// TestHeaderNotices runs build.sh's header_notices, which writes the license files of nv-codec-headers and the AMF
// headers, under the script's own shell options (sourcing it applies set -Eeuo pipefail) over headers larger than a
// pipe buffer, as nvEncodeAPI.h (291 KB) is: a /* */ notice with CRLF line ends and a // notice.
func TestHeaderNotices(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	body := strings.Repeat("#define ISSHONI_TEST_PADDING_VALUE_OF_A_LONG_HEADER 0x0123456789abcdef\n", 2200) // > 128 KiB
	block := strings.ReplaceAll("/*\n * Copyright (c) 2026 Block Notice Inc.\n *\n * Permission is hereby granted.\n */\n"+body, "\n", "\r\n")
	slashes := "// Copyright (c) 2026 Slash Notice Inc.\n//\n// Permission is hereby granted.\n" + body
	writeFile(t, filepath.Join(dir, "a", "block.h"), block)
	writeFile(t, filepath.Join(dir, "a", "block2.h"), block)
	writeFile(t, filepath.Join(dir, "slashes.h"), slashes)
	if len(block) <= 128<<10 || len(slashes) <= 128<<10 {
		t.Fatalf("headers of %d and %d bytes, want more than 128 KiB", len(block), len(slashes))
	}

	run := func() (string, string, error) {
		cmd := exec.CommandContext(t.Context(), bash, "-c", `. native/ffmpeg/build.sh && header_notices "$1" "$2"`,
			"bash", dir, "Test headers: MIT.")
		cmd.Dir = repoRoot(t)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		return string(out), stderr.String(), err
	}
	out, stderr, err := run()
	if err != nil {
		t.Fatalf("header_notices: %v; stderr %q", err, stderr)
	}
	for _, want := range []string{
		"Test headers: MIT.\n",
		"Files:\n  a/block.h\n  a/block2.h\n\nCopyright (c) 2026 Block Notice Inc.\n\nPermission is hereby granted.\n",
		"Files:\n  slashes.h\n\nCopyright (c) 2026 Slash Notice Inc.\n\nPermission is hereby granted.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\r") || strings.Contains(out, "ISSHONI_TEST_PADDING") {
		t.Errorf("the output has a CR or text after the notice:\n%s", out)
	}

	// A header without a notice fails the build.
	writeFile(t, filepath.Join(dir, "bare.h"), body)
	if _, stderr, err = run(); err == nil || !strings.Contains(stderr, "bare.h has no license notice") {
		t.Errorf("a header without a notice: %v, stderr %q", err, stderr)
	}
}

func TestFFmpegInputsCommand(t *testing.T) {
	root := t.TempDir()
	writeFFmpegInputs(t, root)
	want, err := inputsSHA256(root)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(t, "-root", root, "ffmpeg-inputs")
	if code != 0 || strings.TrimSpace(stdout) != want {
		t.Fatalf("exit %d, stdout %q, stderr %q; want %s", code, stdout, stderr, want)
	}

	code, _, stderr = runCmd(t, "-root", root, "ffmpeg-inputs", "-check")
	if code != 1 || !strings.Contains(stderr, errNoLock.Error()) {
		t.Errorf("-check without lock.json: exit %d, %q", code, stderr)
	}

	lock := sampleLock(want)
	writeFile(t, filepath.Join(root, lockPath), lock)
	if code, stdout, stderr = runCmd(t, "-root", root, "ffmpeg-inputs", "-check"); code != 0 || strings.TrimSpace(stdout) != want {
		t.Errorf("-check with a current lock: exit %d, %q, %q", code, stdout, stderr)
	}

	// A pins.json change without a new lock: the consumers' stale failure, word for word (06 §4.1).
	pins := filepath.Join(root, pinsPath)
	data, err := os.ReadFile(pins)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, pins, strings.Replace(string(data), `"revision": 1`, `"revision": 2`, 1))
	code, _, stderr = runCmd(t, "-root", root, "ffmpeg-inputs", "-check")
	if want := "native/ffmpeg/lock.json is stale: run the FFmpeg workflow with publish and update the lock"; code != 1 || !strings.Contains(stderr, want) {
		t.Errorf("-check with a stale lock: exit %d, %q", code, stderr)
	}
	if code, _, _ = runCmd(t, "-root", root, "ffmpeg-inputs"); code != 0 {
		t.Errorf("without -check a stale lock is not an error: exit %d", code)
	}

	writeFile(t, pins, "{}")
	if code, _, stderr = runCmd(t, "-root", root, "ffmpeg-inputs"); code != 1 || !strings.Contains(stderr, "pins.json") {
		t.Errorf("a broken pins.json: exit %d, %q", code, stderr)
	}
}

func TestParseLock(t *testing.T) {
	good := sampleLock(strings.Repeat("a", 64))
	l, err := parseLock([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if l.AvcodecDLL() != "avcodec-63.dll" || l.AvutilDLL() != "avutil-61.dll" {
		t.Errorf("DLL names %s %s", l.AvcodecDLL(), l.AvutilDLL())
	}
	for _, tc := range []struct{ name, old, new, want string }{
		{"tag", `"tag": "deps-ffmpeg-9.0.2-r1"`, `"tag": "v9.0.2"`, "tag"},
		{"asset name", `ffmpeg-9.0.2-r1-win-x64.zip`, `ffmpeg-win.zip`, "assets must be exactly"},
		{"dll name", `"avcodec-63.dll"`, `"avcodec-62.dll"`, "dlls must be exactly"},
		{"major", `"avutilMajor": 61`, `"avutilMajor": 0`, "avutilMajor"},
		{"toolset", `"msvcToolset": "14.44.35207"`, `"msvcToolset": "14.44"`, "msvcToolset"},
		{"commit", `"commit": "0123456789abcdef0123456789abcdef01234567"`, `"commit": "main"`, "commit"},
		{"unknown field", `"revision": 1,`, `"revision": 1, "x": 1,`, "unknown field"},
		{"inputs", `"inputsSha256": "aaaa`, `"inputsSha256": "AAAA`, "inputsSha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := strings.Replace(good, tc.old, tc.new, 1)
			if data == good {
				t.Fatalf("the replacement of %q changed nothing", tc.old)
			}
			if _, err := parseLock([]byte(data)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestCurrentLock(t *testing.T) {
	pins, err := parsePins([]byte(goodPins))
	if err != nil {
		t.Fatal(err)
	}
	inputs := strings.Repeat("b", 64)
	lock, err := parseLock([]byte(sampleLock(inputs)))
	if err != nil {
		t.Fatal(err)
	}
	st := &ffmpegState{pins: pins, inputs: inputs, lock: lock}
	if _, err := st.currentLock(); err != nil {
		t.Errorf("current lock: %v", err)
	}
	st.inputs = strings.Repeat("c", 64)
	if _, err := st.currentLock(); !errors.Is(err, errStaleLock) {
		t.Errorf("changed inputs: %v, want errStaleLock", err)
	}
	st.inputs = inputs
	pins.Revision = 2
	if _, err := st.currentLock(); err == nil || !strings.Contains(err.Error(), "does not match pins.json") {
		t.Errorf("another revision: %v", err)
	}
	st.lock = nil
	if _, err := st.currentLock(); !errors.Is(err, errNoLock) {
		t.Errorf("no lock: %v, want errNoLock", err)
	}
}

// TestCheckFFmpegConfig runs 06 §4.4's checks over ffcheck outputs: a sample of the shipped build passes; real
// ffcheck outputs of two macOS builds of the pinned FFmpeg (an LGPL one without the Windows encoders, a GPL one with
// a decoder) fail for the right reasons.
func TestCheckFFmpegConfig(t *testing.T) {
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	ok := read("ffmpeg-config-ok.json")
	c, err := checkFFmpegConfig([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if c.AvcodecMajor != 63 || c.AvutilMajor != 61 {
		t.Errorf("majors %d %d", c.AvcodecMajor, c.AvutilMajor)
	}

	for _, tc := range []struct {
		name, data string
		want       []string
	}{
		{"macOS LGPL build", read("ffmpeg-config-lgpl-macos.json"), []string{"the encoders are [rawvideo]", `"ok" is not true`}},
		{"macOS GPL build", read("ffmpeg-config-gpl-macos.json"), []string{
			`license is "GPL version 2 or later"`, `avutilLicense is "GPL version 2 or later"`,
			"contains --enable-gpl", "decoders: [rawvideo]",
		}},
		{"nonfree", strings.Replace(ok, "--disable-nonfree", "--enable-nonfree", 1), []string{"contains --enable-nonfree"}},
		{"version3", strings.Replace(ok, "--disable-version3", "--enable-version3", 1), []string{"contains --enable-version3"}},
		{"extra encoder", strings.Replace(ok, `"h264_qsv"]`, `"h264_qsv", "libx264"]`, 1), []string{"not exactly"}},
		{"missing encoder", strings.Replace(ok, `, "h264_qsv"]`, `]`, 1), []string{"not exactly"}},
		{"no majors", strings.Replace(ok, `"avcodecMajor": 63`, `"avcodecMajor": 0`, 1), []string{"avcodecMajor or avutilMajor"}},
		{"not JSON", "{", []string{"ffmpeg-config.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.data == ok {
				t.Fatal("the case changed nothing")
			}
			_, err := checkFFmpegConfig([]byte(tc.data))
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
	// The encoder order does not matter.
	shuffled := strings.Replace(ok, `["h264_amf", "h264_mf", "h264_nvenc", "h264_qsv"]`, `["h264_qsv", "h264_nvenc", "h264_mf", "h264_amf"]`, 1)
	if _, err := checkFFmpegConfig([]byte(shuffled)); err != nil {
		t.Errorf("another encoder order: %v", err)
	}
}

// sampleLock is a lock.json for goodPins, with inputsSha256 set to inputs.
func sampleLock(inputs string) string {
	return `{
  "revision": 1,
  "tag": "deps-ffmpeg-9.0.2-r1",
  "inputsSha256": "` + inputs + `",
  "assets": {
    "ffmpeg-9.0.2-r1-win-x64.zip": "` + strings.Repeat("1", 64) + `",
    "ffmpeg-9.0.2-r1-source.tar": "` + strings.Repeat("2", 64) + `"
  },
  "avcodecMajor": 63,
  "avutilMajor": 61,
  "dlls": {"avcodec-63.dll": "` + strings.Repeat("3", 64) + `", "avutil-61.dll": "` + strings.Repeat("4", 64) + `"},
  "msvcToolset": "14.44.35207",
  "commit": "0123456789abcdef0123456789abcdef01234567"
}
`
}

// writeFFmpegInputs writes the inputs of inputsSha256 under root: goodPins as pins.json (not the committed pins, so
// a bump of pins.json needs no test change) and the committed build.sh and ffcheck.c.
func writeFFmpegInputs(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, pinsPath), goodPins)
	copyTree(t, filepath.Join(repoRoot(t), "native", "ffmpeg"), filepath.Join(root, "native", "ffmpeg"),
		"build.sh", "ffcheck.c")
}

// copyTree copies the named files of src into dst.
func copyTree(t *testing.T, src, dst string, names ...string) {
	t.Helper()
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dst, n), string(data))
	}
}
