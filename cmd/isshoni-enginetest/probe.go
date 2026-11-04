package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	register(command{name: "probe", summary: "print the build info and the caps; write probe.json (-quick: no test encodes)",
		run: cmdProbe})
}

// cmdProbe is 04 §14's probe: the engine's build info and im_probe's caps, printed and written to DIR/probe.json.
// Asserts: a full probe takes less than 2 s, and every encoder has a status.
func cmdProbe(_ context.Context, g *globals, args []string) int {
	fs := newFlags(g, "probe", "[-quick] [-out DIR]")
	quick := fs.Bool("quick", false, "IM_PROBE_QUICK: which encoders the build has, no test encodes")
	out := fs.String("out", ".", "write probe.json into `DIR`")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	e, code, err := g.openEngine(false)
	if err != nil {
		fmt.Fprintln(g.stderr, "probe: the engine didn't load:", err)
		return code
	}
	defer func() { _ = e.Close() }()

	flags := uint32(imProbeFull)
	if *quick {
		flags = imProbeQuick
	}
	start := time.Now()
	caps, err := rawProbe(g.dll, flags)
	wall := time.Since(start)
	if err != nil {
		fmt.Fprintln(g.stderr, "probe:", err)
		return exitFail
	}
	doc := decodeCaps(caps, !*quick)
	doc.WallMS = wall.Milliseconds()
	doc.Build = e.BuildInfo()

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fmt.Fprintln(g.stderr, "probe:", err)
		return exitFail
	}
	if err := os.MkdirAll(*out, 0o750); err != nil {
		fmt.Fprintln(g.stderr, "probe:", err)
		return exitEnv
	}
	if err := os.WriteFile(filepath.Join(*out, "probe.json"), append(data, '\n'), 0o600); err != nil {
		fmt.Fprintln(g.stderr, "probe:", err)
		return exitEnv
	}
	if g.json {
		_, _ = g.stdout.Write(append(data, '\n'))
	} else {
		printProbe(g.stdout, doc)
	}
	fails := checkProbe(doc)
	for _, f := range fails {
		fmt.Fprintln(g.stderr, "FAIL:", f)
	}
	if len(fails) > 0 {
		return exitFail
	}
	return exitPass
}

func printProbe(w io.Writer, d probeDoc) {
	if b, err := json.Marshal(d.Build); err == nil {
		fmt.Fprintf(w, "build: %s\n", b)
	}
	kind := "full"
	if !d.Full {
		kind = "quick"
	}
	fmt.Fprintf(w, "probe (%s): %d ms (engine %d ms)\n", kind, d.WallMS, d.ProbeMS)
	fmt.Fprintf(w, "os: build %d.%d, flags %s, audio flags 0x%x\n", d.OSBuild, d.OSUBR, strings.Join(d.OSFlags, " "),
		d.AudioFlags)
	for _, a := range d.Adapters {
		fmt.Fprintf(w, "adapter %d: %s (vendor %s, device %s, driver %s, software %t, outputs %t)\n", a.Index, a.Name,
			a.VendorID, a.DeviceID, a.Driver, a.Software, a.HasOutputs)
	}
	for _, e := range d.Encoders {
		state := "available"
		if !e.Available {
			state = fmt.Sprintf("unavailable (%d %s)", e.Status, e.Reason)
		}
		fmt.Fprintf(w, "encoder %-8s adapter %2d  %-30s %-60s test %d us\n", e.Name, e.Adapter, state,
			strings.Join(e.Flags, " "), e.TestEncodeUS)
	}
}
