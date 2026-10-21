package native

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
	"unsafe"
)

// goldenLayout is native/common/tests/testdata/abi_layout.json: the struct layouts of 04 §5.4, written once in WA03
// from those numbers and read by the C++ layout test, this test and the Windows DLL test (im_abi_layout).
const goldenLayout = "../../../native/common/tests/testdata/abi_layout.json"

type abiLayout struct {
	ABI     uint32                  `json:"abi"`
	Structs map[string]structLayout `json:"structs"`
}

type structLayout struct {
	Size   uintptr            `json:"size"`
	Fields map[string]uintptr `json:"fields"`
}

// goLayouts are the Go mirrors' layouts, by C struct name: unsafe.Sizeof and unsafe.Offsetof of every field.
func goLayouts() map[string]goLayout {
	return map[string]goLayout{
		"im_init_params": {imInitParams{}, map[string]uintptr{
			"struct_size":   unsafe.Offsetof(imInitParams{}.structSize),
			"abi_version":   unsafe.Offsetof(imInitParams{}.abiVersion),
			"flags":         unsafe.Offsetof(imInitParams{}.flags),
			"log_level":     unsafe.Offsetof(imInitParams{}.logLevel),
			"ffmpeg_dir":    unsafe.Offsetof(imInitParams{}.ffmpegDir),
			"openh264_path": unsafe.Offsetof(imInitParams{}.openh264Path),
		}},
		"im_clock_sample": {imClockSample{}, map[string]uintptr{
			"struct_size":    unsafe.Offsetof(imClockSample{}.structSize),
			"reserved":       unsafe.Offsetof(imClockSample{}.reserved),
			"mono_ns":        unsafe.Offsetof(imClockSample{}.monoNS),
			"unix_ns":        unsafe.Offsetof(imClockSample{}.unixNS),
			"uncertainty_ns": unsafe.Offsetof(imClockSample{}.uncertaintyNS),
		}},
		"im_adapter_info": {imAdapterInfo{}, map[string]uintptr{
			"luid":           unsafe.Offsetof(imAdapterInfo{}.luid),
			"vendor_id":      unsafe.Offsetof(imAdapterInfo{}.vendorID),
			"device_id":      unsafe.Offsetof(imAdapterInfo{}.deviceID),
			"driver_version": unsafe.Offsetof(imAdapterInfo{}.driverVersion),
			"name":           unsafe.Offsetof(imAdapterInfo{}.name),
			"flags":          unsafe.Offsetof(imAdapterInfo{}.flags),
			"reserved":       unsafe.Offsetof(imAdapterInfo{}.reserved),
		}},
		"im_encoder_info": {imEncoderInfo{}, map[string]uintptr{
			"kind":           unsafe.Offsetof(imEncoderInfo{}.kind),
			"adapter_index":  unsafe.Offsetof(imEncoderInfo{}.adapterIndex),
			"flags":          unsafe.Offsetof(imEncoderInfo{}.flags),
			"status":         unsafe.Offsetof(imEncoderInfo{}.status),
			"max_width":      unsafe.Offsetof(imEncoderInfo{}.maxWidth),
			"max_height":     unsafe.Offsetof(imEncoderInfo{}.maxHeight),
			"test_encode_us": unsafe.Offsetof(imEncoderInfo{}.testEncodeUS),
			"reserved":       unsafe.Offsetof(imEncoderInfo{}.reserved),
			"codec":          unsafe.Offsetof(imEncoderInfo{}.codec),
			"reason":         unsafe.Offsetof(imEncoderInfo{}.reason),
		}},
		"im_caps": {imCaps{}, map[string]uintptr{
			"struct_size":   unsafe.Offsetof(imCaps{}.structSize),
			"os_build":      unsafe.Offsetof(imCaps{}.osBuild),
			"os_ubr":        unsafe.Offsetof(imCaps{}.osUBR),
			"os_flags":      unsafe.Offsetof(imCaps{}.osFlags),
			"audio_flags":   unsafe.Offsetof(imCaps{}.audioFlags),
			"adapter_count": unsafe.Offsetof(imCaps{}.adapterCount),
			"adapters":      unsafe.Offsetof(imCaps{}.adapters),
			"encoder_count": unsafe.Offsetof(imCaps{}.encoderCount),
			"reserved0":     unsafe.Offsetof(imCaps{}.reserved0),
			"encoders":      unsafe.Offsetof(imCaps{}.encoders),
			"probe_ms":      unsafe.Offsetof(imCaps{}.probeMS),
			"reserved":      unsafe.Offsetof(imCaps{}.reserved),
		}},
		"im_pick_params": {imPickParams{}, map[string]uintptr{
			"struct_size":   unsafe.Offsetof(imPickParams{}.structSize),
			"flags":         unsafe.Offsetof(imPickParams{}.flags),
			"parent_window": unsafe.Offsetof(imPickParams{}.parentWindow),
			"timeout_ms":    unsafe.Offsetof(imPickParams{}.timeoutMS),
			"reserved":      unsafe.Offsetof(imPickParams{}.reserved),
		}},
		"im_source_info": {imSourceInfo{}, map[string]uintptr{
			"struct_size":  unsafe.Offsetof(imSourceInfo{}.structSize),
			"kind":         unsafe.Offsetof(imSourceInfo{}.kind),
			"id":           unsafe.Offsetof(imSourceInfo{}.id),
			"name":         unsafe.Offsetof(imSourceInfo{}.name),
			"monitor_id":   unsafe.Offsetof(imSourceInfo{}.monitorID),
			"width":        unsafe.Offsetof(imSourceInfo{}.width),
			"height":       unsafe.Offsetof(imSourceInfo{}.height),
			"capture_path": unsafe.Offsetof(imSourceInfo{}.capturePath),
			"flags":        unsafe.Offsetof(imSourceInfo{}.flags),
			"pid":          unsafe.Offsetof(imSourceInfo{}.pid),
			"reserved0":    unsafe.Offsetof(imSourceInfo{}.reserved0),
			"window":       unsafe.Offsetof(imSourceInfo{}.window),
			"reserved":     unsafe.Offsetof(imSourceInfo{}.reserved),
		}},
		"im_rule": {imRule{}, map[string]uintptr{
			"kind":   unsafe.Offsetof(imRule{}.kind),
			"source": unsafe.Offsetof(imRule{}.source),
			"value":  unsafe.Offsetof(imRule{}.value),
			"app_id": unsafe.Offsetof(imRule{}.appID),
			"name":   unsafe.Offsetof(imRule{}.name),
		}},
		"im_exclusions": {imExclusions{}, map[string]uintptr{
			"struct_size": unsafe.Offsetof(imExclusions{}.structSize),
			"flags":       unsafe.Offsetof(imExclusions{}.flags),
			"rules":       unsafe.Offsetof(imExclusions{}.rules),
			"rule_count":  unsafe.Offsetof(imExclusions{}.ruleCount),
			"shared_pid":  unsafe.Offsetof(imExclusions{}.sharedPID),
		}},
		"im_audio_config": {imAudioConfig{}, map[string]uintptr{
			"mode":        unsafe.Offsetof(imAudioConfig{}.mode),
			"bitrate_bps": unsafe.Offsetof(imAudioConfig{}.bitrateBPS),
			"frame_ms":    unsafe.Offsetof(imAudioConfig{}.frameMS),
			"flags":       unsafe.Offsetof(imAudioConfig{}.flags),
			"reserved":    unsafe.Offsetof(imAudioConfig{}.reserved),
		}},
		"im_audio_level": {imAudioLevel{}, map[string]uintptr{
			"app_id":   unsafe.Offsetof(imAudioLevel{}.appID),
			"peak_db":  unsafe.Offsetof(imAudioLevel{}.peakDB),
			"rms_db":   unsafe.Offsetof(imAudioLevel{}.rmsDB),
			"source":   unsafe.Offsetof(imAudioLevel{}.source),
			"reserved": unsafe.Offsetof(imAudioLevel{}.reserved),
		}},
		"im_audio_levels": {imAudioLevels{}, map[string]uintptr{
			"struct_size": unsafe.Offsetof(imAudioLevels{}.structSize),
			"report_seq":  unsafe.Offsetof(imAudioLevels{}.reportSeq),
			"qpc_ns":      unsafe.Offsetof(imAudioLevels{}.qpcNS),
			"mix_peak_db": unsafe.Offsetof(imAudioLevels{}.mixPeakDB),
			"mix_rms_db":  unsafe.Offsetof(imAudioLevels{}.mixRMSDB),
			"count":       unsafe.Offsetof(imAudioLevels{}.count),
			"reserved":    unsafe.Offsetof(imAudioLevels{}.reserved),
			"apps":        unsafe.Offsetof(imAudioLevels{}.apps),
		}},
		"im_layer_config": {imLayerConfig{}, map[string]uintptr{
			"rid":         unsafe.Offsetof(imLayerConfig{}.rid),
			"width":       unsafe.Offsetof(imLayerConfig{}.width),
			"height":      unsafe.Offsetof(imLayerConfig{}.height),
			"max_pixels":  unsafe.Offsetof(imLayerConfig{}.maxPixels),
			"fps":         unsafe.Offsetof(imLayerConfig{}.fps),
			"bitrate_bps": unsafe.Offsetof(imLayerConfig{}.bitrateBPS),
			"gop_ms":      unsafe.Offsetof(imLayerConfig{}.gopMS),
			"flags":       unsafe.Offsetof(imLayerConfig{}.flags),
			"reserved":    unsafe.Offsetof(imLayerConfig{}.reserved),
		}},
		"im_start_config": {imStartConfig{}, map[string]uintptr{
			"struct_size":  unsafe.Offsetof(imStartConfig{}.structSize),
			"flags":        unsafe.Offsetof(imStartConfig{}.flags),
			"source_id":    unsafe.Offsetof(imStartConfig{}.sourceID),
			"profile":      unsafe.Offsetof(imStartConfig{}.profile),
			"content_hint": unsafe.Offsetof(imStartConfig{}.contentHint),
			"encoder":      unsafe.Offsetof(imStartConfig{}.encoder),
			"layer_count":  unsafe.Offsetof(imStartConfig{}.layerCount),
			"layers":       unsafe.Offsetof(imStartConfig{}.layers),
			"audio":        unsafe.Offsetof(imStartConfig{}.audio),
			"exclusions":   unsafe.Offsetof(imStartConfig{}.exclusions),
			"reserved":     unsafe.Offsetof(imStartConfig{}.reserved),
		}},
		"im_layer_info": {imLayerInfo{}, map[string]uintptr{
			"rid":         unsafe.Offsetof(imLayerInfo{}.rid),
			"encoder":     unsafe.Offsetof(imLayerInfo{}.encoder),
			"width":       unsafe.Offsetof(imLayerInfo{}.width),
			"height":      unsafe.Offsetof(imLayerInfo{}.height),
			"fps":         unsafe.Offsetof(imLayerInfo{}.fps),
			"bitrate_bps": unsafe.Offsetof(imLayerInfo{}.bitrateBPS),
			"profile":     unsafe.Offsetof(imLayerInfo{}.profile),
			"flags":       unsafe.Offsetof(imLayerInfo{}.flags),
			"reserved":    unsafe.Offsetof(imLayerInfo{}.reserved),
		}},
		"im_session_info": {imSessionInfo{}, map[string]uintptr{
			"struct_size":  unsafe.Offsetof(imSessionInfo{}.structSize),
			"flags":        unsafe.Offsetof(imSessionInfo{}.flags),
			"capture_path": unsafe.Offsetof(imSessionInfo{}.capturePath),
			"layer_count":  unsafe.Offsetof(imSessionInfo{}.layerCount),
			"epoch_ns":     unsafe.Offsetof(imSessionInfo{}.epochNS),
			"audio_state":  unsafe.Offsetof(imSessionInfo{}.audioState),
			"reserved0":    unsafe.Offsetof(imSessionInfo{}.reserved0),
			"layers":       unsafe.Offsetof(imSessionInfo{}.layers),
			"reserved":     unsafe.Offsetof(imSessionInfo{}.reserved),
		}},
		"im_packet": {imPacket{}, map[string]uintptr{
			"struct_size":   unsafe.Offsetof(imPacket{}.structSize),
			"kind":          unsafe.Offsetof(imPacket{}.kind),
			"layer":         unsafe.Offsetof(imPacket{}.layer),
			"flags":         unsafe.Offsetof(imPacket{}.flags),
			"capture_ts_ns": unsafe.Offsetof(imPacket{}.captureTSNS),
			"data":          unsafe.Offsetof(imPacket{}.data),
			"size":          unsafe.Offsetof(imPacket{}.size),
			"duration_us":   unsafe.Offsetof(imPacket{}.durationUS),
			"width":         unsafe.Offsetof(imPacket{}.width),
			"height":        unsafe.Offsetof(imPacket{}.height),
			"profile":       unsafe.Offsetof(imPacket{}.profile),
			"event":         unsafe.Offsetof(imPacket{}.event),
			"status":        unsafe.Offsetof(imPacket{}.status),
			"reserved":      unsafe.Offsetof(imPacket{}.reserved),
		}},
	}
}

type goLayout struct {
	value  any // a zero value of the mirror
	fields map[string]uintptr
}

func readGoldenLayout(t *testing.T) abiLayout {
	t.Helper()
	b, err := os.ReadFile(goldenLayout)
	if err != nil {
		t.Fatal(err)
	}
	var l abiLayout
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatalf("%s: %v", goldenLayout, err)
	}
	return l
}

// TestLayout: every Go mirror of abi.go has the size and field offsets of the golden file (04 §5.4), and the
// golden file has a mirror for every struct of the header. The numbers are for 64-bit targets (amd64, arm64).
func TestLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the v1 layouts are those of 64-bit targets (04 §5.4)")
	}
	golden := readGoldenLayout(t)
	compareLayouts(t, "golden file", golden)
}

// reporter is the part of testing.T that compareLayouts needs.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// compareLayouts compares the mirrors with a layout document: the golden file, or the DLL's im_abi_layout
// (dll_windows_test.go).
func compareLayouts(t reporter, what string, doc abiLayout) {
	t.Helper()
	if doc.ABI != imABIVersion {
		t.Errorf("%s: abi %#x, want %#x", what, doc.ABI, imABIVersion)
	}
	mirrors := goLayouts()
	for _, name := range sortedKeys(doc.Structs) {
		if _, ok := mirrors[name]; !ok {
			t.Errorf("%s: %s has no Go mirror in abi.go", what, name)
		}
	}
	for _, name := range sortedKeys(mirrors) {
		m := mirrors[name]
		want, ok := doc.Structs[name]
		if !ok {
			t.Errorf("%s: no %s (a Go mirror without a C struct)", what, name)
			continue
		}
		typ := reflect.TypeOf(m.value)
		if got := typ.Size(); got != want.Size {
			t.Errorf("%s: sizeof(%s) = %d in Go (%s), want %d", what, name, got, typ.Name(), want.Size)
		}
		// Every Go field is in the table above, and its c tag names the C field it is at.
		if typ.NumField() != len(m.fields) {
			t.Errorf("%s: %s has %d fields, the test table %d", what, typ.Name(), typ.NumField(), len(m.fields))
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			if off, ok := m.fields[f.Tag.Get("c")]; !ok || off != f.Offset {
				t.Errorf("%s: %s.%s: c tag %q does not match the test table", what, typ.Name(), f.Name, f.Tag.Get("c"))
			}
		}
		for _, field := range sortedKeys(want.Fields) {
			got, ok := m.fields[field]
			switch {
			case !ok:
				t.Errorf("%s: %s.%s has no Go field", what, name, field)
			case got != want.Fields[field]:
				t.Errorf("%s: offsetof(%s, %s) = %d in Go, want %d", what, name, field, got, want.Fields[field])
			}
		}
		for _, field := range sortedKeys(m.fields) {
			if _, ok := want.Fields[field]; !ok {
				t.Errorf("%s: Go's %s has %s, which the C struct lacks", what, name, field)
			}
		}
	}
}

// TestLayoutSpecNumbers pins 04 §5.4's table itself, so that a golden file edited together with the mirrors still
// fails.
func TestLayoutSpecNumbers(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the v1 layouts are those of 64-bit targets (04 §5.4)")
	}
	for _, c := range []struct {
		what      string
		got, want uintptr
	}{
		{"im_init_params", unsafe.Sizeof(imInitParams{}), 32},
		{"im_init_params.ffmpeg_dir", unsafe.Offsetof(imInitParams{}.ffmpegDir), 16},
		{"im_init_params.openh264_path", unsafe.Offsetof(imInitParams{}.openh264Path), 24},
		{"im_clock_sample", unsafe.Sizeof(imClockSample{}), 32},
		{"im_adapter_info", unsafe.Sizeof(imAdapterInfo{}), 160},
		{"im_adapter_info.name", unsafe.Offsetof(imAdapterInfo{}.name), 24},
		{"im_encoder_info", unsafe.Sizeof(imEncoderInfo{}), 104},
		{"im_encoder_info.codec", unsafe.Offsetof(imEncoderInfo{}.codec), 40},
		{"im_encoder_info.reason", unsafe.Offsetof(imEncoderInfo{}.reason), 72},
		{"im_caps", unsafe.Sizeof(imCaps{}), 1568},
		{"im_caps.adapters", unsafe.Offsetof(imCaps{}.adapters), 24},
		{"im_caps.encoder_count", unsafe.Offsetof(imCaps{}.encoderCount), 664},
		{"im_caps.encoders", unsafe.Offsetof(imCaps{}.encoders), 672},
		{"im_caps.probe_ms", unsafe.Offsetof(imCaps{}.probeMS), 1504},
		{"im_pick_params", unsafe.Sizeof(imPickParams{}), 24},
		{"im_pick_params.parent_window", unsafe.Offsetof(imPickParams{}.parentWindow), 8},
		{"im_source_info", unsafe.Sizeof(imSourceInfo{}), 440},
		{"im_source_info.id", unsafe.Offsetof(imSourceInfo{}.id), 8},
		{"im_source_info.name", unsafe.Offsetof(imSourceInfo{}.name), 72},
		{"im_source_info.monitor_id", unsafe.Offsetof(imSourceInfo{}.monitorID), 328},
		{"im_source_info.width", unsafe.Offsetof(imSourceInfo{}.width), 392},
		{"im_source_info.pid", unsafe.Offsetof(imSourceInfo{}.pid), 408},
		{"im_source_info.window", unsafe.Offsetof(imSourceInfo{}.window), 416},
		{"im_rule", unsafe.Sizeof(imRule{}), 32},
		{"im_rule.value", unsafe.Offsetof(imRule{}.value), 8},
		{"im_rule.app_id", unsafe.Offsetof(imRule{}.appID), 16},
		{"im_rule.name", unsafe.Offsetof(imRule{}.name), 24},
		{"im_exclusions", unsafe.Sizeof(imExclusions{}), 24},
		{"im_exclusions.rules", unsafe.Offsetof(imExclusions{}.rules), 8},
		{"im_exclusions.rule_count", unsafe.Offsetof(imExclusions{}.ruleCount), 16},
		{"im_exclusions.shared_pid", unsafe.Offsetof(imExclusions{}.sharedPID), 20},
		{"im_audio_config", unsafe.Sizeof(imAudioConfig{}), 32},
		{"im_audio_config.mode", unsafe.Offsetof(imAudioConfig{}.mode), 0},
		{"im_audio_config.bitrate_bps", unsafe.Offsetof(imAudioConfig{}.bitrateBPS), 4},
		{"im_audio_config.frame_ms", unsafe.Offsetof(imAudioConfig{}.frameMS), 8},
		{"im_audio_config.flags", unsafe.Offsetof(imAudioConfig{}.flags), 12},
		{"im_audio_level", unsafe.Sizeof(imAudioLevel{}), 80},
		{"im_audio_level.peak_db", unsafe.Offsetof(imAudioLevel{}.peakDB), 64},
		{"im_audio_level.rms_db", unsafe.Offsetof(imAudioLevel{}.rmsDB), 68},
		{"im_audio_level.source", unsafe.Offsetof(imAudioLevel{}.source), 72},
		{"im_audio_levels", unsafe.Sizeof(imAudioLevels{}), 5152},
		{"im_audio_levels.qpc_ns", unsafe.Offsetof(imAudioLevels{}.qpcNS), 8},
		{"im_audio_levels.mix_peak_db", unsafe.Offsetof(imAudioLevels{}.mixPeakDB), 16},
		{"im_audio_levels.count", unsafe.Offsetof(imAudioLevels{}.count), 24},
		{"im_audio_levels.apps", unsafe.Offsetof(imAudioLevels{}.apps), 32},
		{"im_layer_config", unsafe.Sizeof(imLayerConfig{}), 40},
		{"im_start_config", unsafe.Sizeof(imStartConfig{}), 224},
		{"im_start_config.source_id", unsafe.Offsetof(imStartConfig{}.sourceID), 8},
		{"im_start_config.layers", unsafe.Offsetof(imStartConfig{}.layers), 32},
		{"im_start_config.audio", unsafe.Offsetof(imStartConfig{}.audio), 152},
		{"im_start_config.exclusions", unsafe.Offsetof(imStartConfig{}.exclusions), 184},
		{"im_start_config.reserved", unsafe.Offsetof(imStartConfig{}.reserved), 192},
		{"im_layer_info", unsafe.Sizeof(imLayerInfo{}), 40},
		{"im_session_info", unsafe.Sizeof(imSessionInfo{}), 184},
		{"im_session_info.epoch_ns", unsafe.Offsetof(imSessionInfo{}.epochNS), 16},
		{"im_session_info.layers", unsafe.Offsetof(imSessionInfo{}.layers), 32},
		{"im_packet", unsafe.Sizeof(imPacket{}), 72},
		{"im_packet.capture_ts_ns", unsafe.Offsetof(imPacket{}.captureTSNS), 16},
		{"im_packet.data", unsafe.Offsetof(imPacket{}.data), 24},
		{"im_packet.size", unsafe.Offsetof(imPacket{}.size), 32},
		{"im_packet.width", unsafe.Offsetof(imPacket{}.width), 40},
		{"im_packet.profile", unsafe.Offsetof(imPacket{}.profile), 44},
		{"im_packet.event", unsafe.Offsetof(imPacket{}.event), 48},
		{"im_packet.status", unsafe.Offsetof(imPacket{}.status), 52},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d, 04 §5.4 says %d", c.what, c.got, c.want)
		}
	}
}

// TestLayoutDetectsDrift: the comparison fails for a layout document that differs from the mirrors by one offset or
// one size, or that lacks a struct.
func TestLayoutDetectsDrift(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the v1 layouts are those of 64-bit targets (04 §5.4)")
	}
	for name, mutate := range map[string]func(*abiLayout){
		"offset": func(l *abiLayout) { l.Structs["im_packet"].Fields["status"] = 56 },
		"size": func(l *abiLayout) {
			s := l.Structs["im_caps"]
			s.Size += 8
			l.Structs["im_caps"] = s
		},
		"struct": func(l *abiLayout) { delete(l.Structs, "im_rule") },
		"field":  func(l *abiLayout) { l.Structs["im_rule"].Fields["flags"] = 32 },
		"abi":    func(l *abiLayout) { l.ABI = 0x10001 },
	} {
		t.Run(name, func(t *testing.T) {
			l := readGoldenLayout(t)
			mutate(&l)
			r := &recordingT{}
			compareLayouts(r, "mutated", l)
			if !r.failed {
				t.Errorf("a %s change went unnoticed", name)
			}
		})
	}
}

// recordingT is a reporter that records a failure instead of failing.
type recordingT struct{ failed bool }

func (r *recordingT) Helper()               {}
func (r *recordingT) Errorf(string, ...any) { r.failed = true }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
