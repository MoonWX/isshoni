//go:build ignore

// Command syso writes isshoni.exe's resource object, rsrc_windows_<arch>.syso (docs/m2/06-windows-release.md §5.2
// step 1): the icon, app.manifest byte for byte, and the version resource from an info.json in the format of
// `wails3 generate syso -info`. `task desktop:exe:windows` builds and runs it inside the tools/wails3 module, whose
// dependency graph already pins github.com/tc-hib/winres (0BSD) for wails3's own syso command, so this adds no
// module anywhere:
//
//	go build -C tools/wails3 -o ../../dist-desktop/.build/syso ../../cmd/isshoni-desktop/build/windows/syso.go
//	dist-desktop/.build/syso -arch amd64 -icon … -manifest … -info … -out …
//	dist-desktop/.build/syso -check dist-desktop/windows_amd64/isshoni.exe -manifest …
//
// The -check form fails unless the built exe carries the manifest unchanged.
//
// It replaces `wails3 generate syso`, which parses the manifest into winres.AppManifest and writes that struct back
// out: at Wails v3.0.0-beta.34 (winres v0.3.1) the round trip drops <activeCodePage> and turns "PerMonitorV2,
// PerMonitor" into "permonitorv2,system" (03-desktop-app.md §4.5 asks for both as written). The build tag keeps the
// file out of the root module; the manifest is checked for 03 §4.5's settings before anything is written, and the
// object is checked to carry it unchanged.
package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tc-hib/winres"
	"github.com/tc-hib/winres/version"
)

func main() {
	var arch, icon, manifest, info, out, check string
	flag.StringVar(&check, "check", "", "an exe to check for the manifest instead of writing a .syso")
	flag.StringVar(&arch, "arch", "amd64", "target architecture: amd64 or arm64")
	flag.StringVar(&icon, "icon", "", "the .ico file")
	flag.StringVar(&manifest, "manifest", "", "the application manifest, embedded as it is")
	flag.StringVar(&info, "info", "", "the version resource as JSON (wails3's info.json format)")
	flag.StringVar(&out, "out", "", "the .syso file to write")
	flag.Parse()
	var err error
	if check != "" {
		err = checkExe(check, manifest)
	} else {
		err = run(arch, icon, manifest, info, out)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "syso:", err)
		os.Exit(1)
	}
}

func run(arch, iconPath, manifestPath, infoPath, out string) error {
	if iconPath == "" || manifestPath == "" || infoPath == "" || out == "" {
		return errors.New("-icon, -manifest, -info and -out are required")
	}
	targets := map[string]winres.Arch{"amd64": winres.ArchAMD64, "arm64": winres.ArchARM64}
	target, ok := targets[arch]
	if !ok {
		return fmt.Errorf("arch %q is not amd64 or arm64", arch)
	}

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	if err := checkManifest(manifest); err != nil {
		return fmt.Errorf("%s: %w", manifestPath, err)
	}

	f, err := os.Open(iconPath)
	if err != nil {
		return err
	}
	ico, err := winres.LoadICO(f)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", iconPath, err)
	}

	infoJSON, err := os.ReadFile(infoPath)
	if err != nil {
		return err
	}
	var vi version.Info
	if err := vi.UnmarshalJSON(infoJSON); err != nil {
		return fmt.Errorf("%s: %w", infoPath, err)
	}

	var rs winres.ResourceSet
	if err := rs.SetIcon(winres.ID(1), ico); err != nil {
		return err
	}
	// The manifest as written: resource 1, en-US, as Windows' loader looks it up for an exe.
	if err := rs.Set(winres.RT_MANIFEST, winres.ID(1), winres.LCIDDefault, bytes.Clone(manifest)); err != nil {
		return err
	}
	rs.SetVersionInfo(vi)

	var obj bytes.Buffer
	if err := rs.WriteObject(&obj, target); err != nil {
		return err
	}
	if !bytes.Contains(obj.Bytes(), manifest) {
		return errors.New("the object does not carry the manifest unchanged")
	}
	return os.WriteFile(out, obj.Bytes(), 0o644)
}

// checkExe fails unless the file at exePath holds the manifest at manifestPath byte for byte (resource data is
// copied into the exe's .rsrc section as it is).
func checkExe(exePath, manifestPath string) error {
	if manifestPath == "" {
		return errors.New("-check needs -manifest")
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	exe, err := os.ReadFile(exePath)
	if err != nil {
		return err
	}
	if !bytes.Contains(exe, manifest) {
		return fmt.Errorf("%s does not carry %s unchanged", exePath, manifestPath)
	}
	return nil
}

// required are the manifest settings of 03 §4.5 (and 06 §5.2): element (local name) → attribute ("" for the
// element's text) → value.
var required = []struct{ element, attr, value string }{
	{"requestedExecutionLevel", "level", "asInvoker"},
	{"dpiAware", "", "true/pm"},
	{"dpiAwareness", "", "PerMonitorV2, PerMonitor"},
	{"supportedOS", "Id", "{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"}, // Windows 10 and 11
	{"assemblyIdentity", "name", "Microsoft.Windows.Common-Controls"},
	{"assemblyIdentity", "version", "6.0.0.0"},
	{"activeCodePage", "", "UTF-8"},
	{"longPathAware", "", "true"},
}

// checkManifest fails on a manifest that isn't well-formed UTF-8 XML (Windows then refuses to start the exe) or that
// lacks one of the required settings.
func checkManifest(b []byte) error {
	type seen struct{ element, attr, value string }
	var found []seen
	d := xml.NewDecoder(bytes.NewReader(b))
	var open []string
	var text strings.Builder
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("not well-formed XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			open = append(open, t.Name.Local)
			text.Reset()
			for _, a := range t.Attr {
				found = append(found, seen{t.Name.Local, a.Name.Local, a.Value})
			}
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			found = append(found, seen{t.Name.Local, "", strings.TrimSpace(text.String())})
			open = open[:len(open)-1]
			text.Reset()
		}
	}
	if len(open) != 0 {
		return errors.New("not well-formed XML: unclosed elements")
	}
	for _, r := range required {
		ok := false
		for _, s := range found {
			if s == (seen{r.element, r.attr, r.value}) {
				ok = true
				break
			}
		}
		if !ok {
			if r.attr == "" {
				return fmt.Errorf("no <%s>%s</%s>", r.element, r.value, r.element)
			}
			return fmt.Errorf("no <%s %s=%q>", r.element, r.attr, r.value)
		}
	}
	return nil
}
