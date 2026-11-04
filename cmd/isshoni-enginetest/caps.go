package main

import (
	"bytes"
	"fmt"
)

// The probe as im_probe writes it (native/include/isshoni_media.h; 04 §5.4's layouts, which layout_test.go pins):
// the probe command reads im_caps itself, because it reports what the binding's media.Probe leaves out (the
// adapters, each encoder's status, reason and test_encode_us, probe_ms) and because it runs IM_PROBE_QUICK, which the
// binding never asks for. The binding still loads the engine, initializes it and reads the build info.

const (
	imMaxAdapters = 4
	imMaxEncoders = 8

	imProbeQuick = 0x0
	imProbeFull  = 0x1

	imEncfAvailable   = 0x01
	imEncfHardware    = 0x02
	imEncfLiveBitrate = 0x04
	imEncfZeroCopy    = 0x08
	imEncfHigh        = 0x10
	imEncfCB          = 0x20
	imEncfForcedIDR   = 0x40

	imAdapterSoftware   = 0x1
	imAdapterHasOutputs = 0x2

	imOK = 0
)

type rawAdapter struct {
	LUID          uint64
	VendorID      uint32
	DeviceID      uint32
	DriverVersion uint64
	Name          [128]byte
	Flags         uint32
	Reserved      uint32
}

type rawEncoder struct {
	Kind         uint32
	AdapterIndex uint32
	Flags        uint32
	Status       int32
	MaxWidth     uint32
	MaxHeight    uint32
	TestEncodeUS uint32
	Reserved     [3]uint32
	Codec        [32]byte
	Reason       [32]byte
}

type rawCaps struct {
	StructSize   uint32
	OSBuild      uint32
	OSUBR        uint32
	OSFlags      uint32
	AudioFlags   uint32
	AdapterCount uint32
	Adapters     [imMaxAdapters]rawAdapter
	EncoderCount uint32
	Reserved0    uint32
	Encoders     [imMaxEncoders]rawEncoder
	ProbeMS      uint32
	Reserved     [15]uint32
}

// The encoder kinds' tokens (04 §5.6).
var encoderTokens = map[uint32]string{
	1: "nvenc", 2: "qsv", 3: "amf", 4: "mf-hw", 5: "mf-sw", 6: "openh264", 7: "videotoolbox", 8: "gstreamer",
	100: "fake",
}

// The IM_OS_* flags by name, for the report.
var osFlagNames = []struct {
	bit  uint32
	name string
}{
	{0x001, "supported"}, {0x002, "wgc"}, {0x004, "wgcPicker"}, {0x008, "wgcBorderless"}, {0x010, "wgcMinInterval"},
	{0x020, "wgcSecondaryWindows"}, {0x040, "wgcCursorToggle"}, {0x080, "desktopDuplication"},
	{0x100, "excludeFromCapture"}, {0x200, "mediaFoundation"}, {0x400, "hdrDisplay"},
}

// The probe's JSON (probe.json): every field of im_caps the tool reports, adapter names included (GPU models, not
// machine names), no LUIDs (they change per boot and say nothing to a reader).
type probeDoc struct {
	Full       bool         `json:"full"`
	WallMS     int64        `json:"wallMs"`
	ProbeMS    uint32       `json:"probeMs"`
	OSBuild    uint32       `json:"osBuild"`
	OSUBR      uint32       `json:"osUbr"`
	OSFlags    []string     `json:"osFlags"`
	AudioFlags uint32       `json:"audioFlags"`
	Adapters   []adapterDoc `json:"adapters"`
	Encoders   []encoderDoc `json:"encoders"`
	Build      any          `json:"build,omitempty"`
}

type adapterDoc struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	VendorID   string `json:"vendorId"`
	DeviceID   string `json:"deviceId"`
	Driver     string `json:"driver"`
	Software   bool   `json:"software"`
	HasOutputs bool   `json:"hasOutputs"`
}

type encoderDoc struct {
	Name         string   `json:"name"`
	Codec        string   `json:"codec"`
	Adapter      int      `json:"adapter"` // -1: software
	Available    bool     `json:"available"`
	Status       int32    `json:"status"`
	Reason       string   `json:"reason,omitempty"`
	Flags        []string `json:"flags"`
	TestEncodeUS uint32   `json:"testEncodeUs"`
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// driverVersion formats the UMD version's four 16-bit parts ("32.0.15.6094").
func driverVersion(v uint64) string {
	return fmt.Sprintf("%d.%d.%d.%d", v>>48, (v>>32)&0xFFFF, (v>>16)&0xFFFF, v&0xFFFF)
}

func encoderFlagNames(f uint32) []string {
	names := []string{}
	for _, n := range []struct {
		bit  uint32
		name string
	}{
		{imEncfAvailable, "available"}, {imEncfHardware, "hardware"}, {imEncfLiveBitrate, "liveBitrate"},
		{imEncfZeroCopy, "zeroCopy"}, {imEncfHigh, "high"}, {imEncfCB, "cb"}, {imEncfForcedIDR, "forcedIdr"},
	} {
		if f&n.bit != 0 {
			names = append(names, n.name)
		}
	}
	return names
}

// decodeCaps turns im_caps into the report.
func decodeCaps(c *rawCaps, full bool) probeDoc {
	d := probeDoc{
		Full: full, ProbeMS: c.ProbeMS, OSBuild: c.OSBuild, OSUBR: c.OSUBR, AudioFlags: c.AudioFlags,
		OSFlags: []string{}, Adapters: []adapterDoc{}, Encoders: []encoderDoc{},
	}
	for _, f := range osFlagNames {
		if c.OSFlags&f.bit != 0 {
			d.OSFlags = append(d.OSFlags, f.name)
		}
	}
	for i := range min(int(c.AdapterCount), imMaxAdapters) {
		a := &c.Adapters[i]
		d.Adapters = append(d.Adapters, adapterDoc{
			Index: i, Name: cString(a.Name[:]), VendorID: fmt.Sprintf("0x%04x", a.VendorID),
			DeviceID: fmt.Sprintf("0x%04x", a.DeviceID), Driver: driverVersion(a.DriverVersion),
			Software: a.Flags&imAdapterSoftware != 0, HasOutputs: a.Flags&imAdapterHasOutputs != 0,
		})
	}
	for i := range min(int(c.EncoderCount), imMaxEncoders) {
		e := &c.Encoders[i]
		name, ok := encoderTokens[e.Kind]
		if !ok {
			name = fmt.Sprintf("kind-%d", e.Kind)
		}
		adapter := -1
		if e.AdapterIndex != 0xFFFFFFFF {
			adapter = int(e.AdapterIndex)
		}
		d.Encoders = append(d.Encoders, encoderDoc{
			Name: name, Codec: cString(e.Codec[:]), Adapter: adapter, Available: e.Flags&imEncfAvailable != 0,
			Status: e.Status, Reason: cString(e.Reason[:]), Flags: encoderFlagNames(e.Flags),
			TestEncodeUS: e.TestEncodeUS,
		})
	}
	return d
}

// checkProbe is 04 §14's probe assertion: every encoder has a status (available with IM_OK and no reason, or
// unavailable with a non-zero status and a §5.7 token), and a full probe took less than 2 s. It returns the failures.
func checkProbe(d probeDoc) []string {
	var fails []string
	if d.Full && d.WallMS >= 2000 {
		fails = append(fails, fmt.Sprintf("the full probe took %d ms (04 §9.10: < 2000 ms)", d.WallMS))
	}
	if len(d.Encoders) == 0 {
		fails = append(fails, "the probe lists no encoder")
	}
	for _, e := range d.Encoders {
		switch {
		case e.Status == imOK && e.Reason != "":
			fails = append(fails, fmt.Sprintf("%s: status OK with reason %q", e.Name, e.Reason))
		case e.Status != imOK && e.Reason == "":
			fails = append(fails, fmt.Sprintf("%s: status %d without a reason token", e.Name, e.Status))
		case e.Status != imOK && e.Available:
			fails = append(fails, fmt.Sprintf("%s: available with status %d", e.Name, e.Status))
		case d.Full && e.Status == imOK && !e.Available:
			fails = append(fails, e.Name+": a full probe left it untested with status OK")
		}
	}
	return fails
}
