package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This file is the manual real-app checks' half of the audiotest (docs/m2/05-windows-audio.md §18.2, §18.8,
// §18.9): `record` records the PCM tap while the owner runs a real app (R1–R7), prints what friends hear every
// second, asks "Pass? [y/n]" and stores {label, pass, os} in manual-<label>.json; `merge` adds those entries to a
// gate report, refusing entries from another OS build. Everything here is portable; only the recording itself is
// the driver's (driver_windows.go).

// manualFile is the manual-<label>.json that `record` writes (05 §18.2): the label, the owner's answer and the OS
// build it was recorded on.
type manualFile struct {
	Label string `json:"label"`
	Pass  bool   `json:"pass"`
	OS    osInfo `json:"os"`
}

// labelToken is a valid manual label: "R1", "R7-teams". It becomes part of a file name and of the committed report,
// so it is a short plain word.
var labelToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// manualFileName is the file `record` stores a label's entry in.
func manualFileName(label string) string { return "manual-" + label + ".json" }

// The limits of `record -d`.
const maxRecordSec = 3600

// recordOptions are the parsed flags of `record` (05 §18.2).
type recordOptions struct {
	label   string
	dur     float64
	out     string   // the WAV file
	exclude []string // exe names kept out for this recording (user keep-out rules)
}

// hearStatus is one second of `record`'s console output: what friends hear and what is kept out, by display name.
// Names are local: printed on the console, never written to a file.
type hearStatus struct {
	t       float64
	heard   []string
	keptOut []namedReason
}

// namedReason is a kept-out app's display name and its reason ("builtin", "mic", …).
type namedReason struct {
	name, reason string
}

// recordSink receives what the driver records: the PCM tap, chunk by chunk, and the status every second.
type recordSink interface {
	pcm(left, right []float64) error
	status(st hearStatus)
}

// cmdRecord is the `record` subcommand: it reads the answer from in (the console).
func cmdRecord(w io.Writer, in io.Reader, args []string) int {
	return recordWith(w, in, args, newDriver)
}

// parseRecord parses `record -label R1 [-d SEC] [-o file.wav] [-exclude exe,…]`.
func parseRecord(w io.Writer, args []string) (recordOptions, error) {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.SetOutput(w)
	label := fs.String("label", "", "the manual check's label: R1 … R7-<app> (05 §18.9)")
	dur := fs.Float64("d", 30, "how long to record (s)")
	out := fs.String("o", "", "the WAV file (default: the label in lower case, .wav)")
	exclude := fs.String("exclude", "", "exe names to keep out for this recording, comma-separated")
	if err := fs.Parse(args); err != nil {
		return recordOptions{}, err
	}
	if fs.NArg() > 0 {
		return recordOptions{}, fmt.Errorf("record: unexpected argument %q", fs.Arg(0))
	}
	if !labelToken.MatchString(*label) {
		return recordOptions{}, errors.New("record: -label is required: a short word such as R1 or R7-teams")
	}
	if *dur <= 0 || *dur > maxRecordSec {
		return recordOptions{}, fmt.Errorf("record: -d %g: want (0, %d] seconds", *dur, maxRecordSec)
	}
	ro := recordOptions{label: *label, dur: *dur, out: *out}
	if ro.out == "" {
		ro.out = strings.ToLower(*label) + ".wav"
	}
	for _, exe := range strings.Split(*exclude, ",") {
		if exe = strings.ToLower(strings.TrimSpace(exe)); exe != "" {
			ro.exclude = append(ro.exclude, exe)
		}
	}
	return ro, nil
}

// recordWith is cmdRecord with the driver constructor injected (tests). Exit codes: 0 the owner answered pass, 1 the
// owner answered fail (both stored), 4 a setup error or no answer (nothing stored).
func recordWith(w io.Writer, in io.Reader, args []string, open func(driverOptions) (driver, error)) int {
	ro, err := parseRecord(w, args)
	if err != nil {
		fmt.Fprintln(w, err)
		return exitSetup
	}
	drv, err := open(driverOptions{console: w})
	if err != nil {
		fmt.Fprintln(w, "setup error:", err)
		return exitSetup
	}
	defer func() { _ = drv.close() }()
	_, osi, _, err := drv.env()
	if err != nil {
		fmt.Fprintln(w, "setup error:", err)
		return exitSetup
	}
	ws, err := newWAVStream(ro.out)
	if err != nil {
		fmt.Fprintln(w, "error:", err)
		return exitSetup
	}
	fmt.Fprintf(w, "recording %s for %g s into %s; play the app now\n", ro.label, ro.dur, ro.out)
	recErr := drv.record(ro, &consoleSink{w: w, wav: ws})
	if err := ws.close(); err != nil && recErr == nil {
		recErr = err
	}
	if recErr != nil {
		fmt.Fprintln(w, "error: the recording failed:", recErr)
		return exitSetup
	}
	pass, ok := askPass(w, in, ro.out)
	if !ok {
		fmt.Fprintln(w, "no answer: nothing stored")
		return exitSetup
	}
	path := filepath.Join(filepath.Dir(ro.out), manualFileName(ro.label))
	b, err := json.MarshalIndent(manualFile{Label: ro.label, Pass: pass, OS: osi}, "", "  ")
	if err == nil {
		err = os.WriteFile(path, append(b, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintln(w, "error: the result could not be stored:", err)
		return exitSetup
	}
	fmt.Fprintln(w, "stored:", path)
	if !pass {
		return exitFail
	}
	return exitAllPass
}

// consoleSink writes the recording into the WAV file and prints each second's status.
type consoleSink struct {
	w   io.Writer
	wav *wavStream
}

func (s *consoleSink) pcm(left, right []float64) error { return s.wav.write(left, right) }
func (s *consoleSink) status(st hearStatus)            { fmt.Fprintln(s.w, formatHear(st)) }

// formatHear is one status line of `record` (05 §18.2: "friends hear … / kept out …").
func formatHear(st hearStatus) string {
	heard := "nothing"
	if len(st.heard) > 0 {
		heard = strings.Join(st.heard, ", ")
	}
	kept := "nothing"
	if len(st.keptOut) > 0 {
		parts := make([]string, len(st.keptOut))
		for i, k := range st.keptOut {
			parts[i] = k.name
			if k.reason != "" {
				parts[i] += " (" + k.reason + ")"
			}
		}
		kept = strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%4.0fs  friends hear: %s / kept out: %s", st.t, heard, kept)
}

// askPass asks "Pass? [y/n]" until the answer is yes or no; ok is false at the end of the input.
func askPass(w io.Writer, in io.Reader, wav string) (pass, ok bool) {
	sc := bufio.NewScanner(in)
	for {
		fmt.Fprintf(w, "Listen to %s. Pass? [y/n] ", wav)
		if !sc.Scan() {
			fmt.Fprintln(w)
			return false, false
		}
		switch strings.ToLower(strings.TrimSpace(sc.Text())) {
		case "y", "yes":
			return true, true
		case "n", "no":
			return false, true
		}
	}
}

// ---------------------------------------------------------------- merge

// cmdMerge is the `merge [-o out.json] report.json manual-*.json` subcommand (05 §18.2, §18.8 step 5): it adds the
// manual entries to the report and writes it back in place (or to -o). Patterns are expanded here too, since
// PowerShell passes `manual-*.json` to a program as it is. Exit 0 on success, 4 when it refuses (another OS build,
// a conflicting answer, an unredacted or unreadable report).
func cmdMerge(w io.Writer, args []string) int {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	fs.SetOutput(w)
	out := fs.String("o", "", "write the merged report here instead of over the report")
	if err := fs.Parse(args); err != nil {
		return exitSetup
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(w, "usage: merge [-o out.json] report.json manual-*.json")
		return exitSetup
	}
	reportPath := fs.Arg(0)
	manuals, err := expandArgs(fs.Args()[1:])
	if err != nil {
		fmt.Fprintln(w, "merge:", err)
		return exitSetup
	}
	r, err := readReport(reportPath)
	if err != nil {
		fmt.Fprintln(w, "merge:", err)
		return exitSetup
	}
	var entries []manualFile
	for _, p := range manuals {
		m, err := readManual(p)
		if err != nil {
			fmt.Fprintln(w, "merge:", err)
			return exitSetup
		}
		entries = append(entries, m)
	}
	if err := mergeManual(&r, entries); err != nil {
		fmt.Fprintln(w, "merge:", err)
		return exitSetup
	}
	b, err := encodeReport(r)
	if err != nil {
		fmt.Fprintln(w, "merge:", err)
		return exitSetup
	}
	dst := reportPath
	if *out != "" {
		dst = *out
	}
	if err := writeFileAtomic(dst, b); err != nil {
		fmt.Fprintln(w, "merge:", err)
		return exitSetup
	}
	fmt.Fprintf(w, "merged %d manual entries into %s\n", len(entries), dst)
	return exitAllPass
}

// expandArgs expands the arguments that are patterns (and name no file) into the files they match, in order.
func expandArgs(args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		if _, err := os.Stat(a); err == nil || !strings.ContainsAny(a, "*?[") {
			out = append(out, a)
			continue
		}
		m, err := filepath.Glob(a)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", a, err)
		}
		if len(m) == 0 {
			return nil, fmt.Errorf("no file matches %q", a)
		}
		out = append(out, m...)
	}
	return out, nil
}

// readReport reads a gate report strictly: schema v1, no field this harness doesn't know (a merge must not drop
// what a newer harness wrote), and redacted (05 §18.7): neither named as a -no-redact report nor holding what the
// redaction removes (unredactedProblem).
func readReport(path string) (report, error) {
	if strings.HasSuffix(strings.ToLower(filepath.Base(path)), unredactedSuffix+".json") {
		return report{}, fmt.Errorf("%s: the report was written with -no-redact and must not be committed", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return report{}, err
	}
	var r report
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return report{}, fmt.Errorf("%s: not an audiotest report this harness can read: %w", path, err)
	}
	if r.V != reportVersion {
		return report{}, fmt.Errorf("%s: report schema v%d, want v%d", path, r.V, reportVersion)
	}
	if p := unredactedProblem(r); p != "" {
		return report{}, fmt.Errorf("%s: %s", path, p)
	}
	return r, nil
}

// readManual reads one manual-<label>.json strictly.
func readManual(path string) (manualFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return manualFile{}, err
	}
	var m manualFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return manualFile{}, fmt.Errorf("%s: not a manual entry: %w", path, err)
	}
	if !labelToken.MatchString(m.Label) {
		return manualFile{}, fmt.Errorf("%s: invalid label %q", path, m.Label)
	}
	return m, nil
}

// mergeManual adds the entries to r's manual list (05 §18.8 step 5). It refuses, changing nothing, an entry recorded
// on another OS build than the report's (product, build and UBR) and an entry whose label the report or another
// entry already holds with the other answer; the same label with the same answer is merged once (merging twice
// changes nothing).
func mergeManual(r *report, entries []manualFile) error {
	for _, m := range entries {
		if m.OS.Product != r.OS.Product || m.OS.Build != r.OS.Build || m.OS.UBR != r.OS.UBR {
			return fmt.Errorf("%s was recorded on %s, the report on %s: refusing an entry from another OS build",
				m.Label, osBuildLabel(m.OS), osBuildLabel(r.OS))
		}
	}
	merged := append([]manualEntry(nil), r.Manual...)
	for _, m := range entries {
		dup := false
		for _, e := range merged {
			if e.Label != m.Label {
				continue
			}
			if e.Pass != m.Pass {
				return fmt.Errorf("%s is already in the report with pass=%v; this entry says pass=%v", m.Label,
					e.Pass, m.Pass)
			}
			dup = true
		}
		if !dup {
			merged = append(merged, manualEntry{Label: m.Label, Pass: m.Pass})
		}
	}
	r.Manual = merged
	return nil
}

// osBuildLabel is an OS build for a message: "win11 26200.6899".
func osBuildLabel(o osInfo) string {
	return fmt.Sprintf("%s %d.%d", o.Product, o.Build, o.UBR)
}

// encodeReport is the report's file form: indented JSON with a final newline. HTML characters are not escaped, so
// the redaction's placeholders read "<name>", "<path>", "<home>" and "<harness>" for the owner who checks the file
// before it is committed.
func encodeReport(r report) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileAtomic writes data to path through a temporary file in the same folder, so a failed write never leaves
// half a report.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".audiotest-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}
