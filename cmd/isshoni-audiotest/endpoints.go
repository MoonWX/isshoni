package main

import "strings"

// This file names render endpoints the way the report may (docs/m2/05-windows-audio.md §18.7: "endpoint names become
// {formFactor, bus}"): the EndpointFormFactor enum by its name, and the device's enumerator (the bus driver) as one
// of a few buses. Friendly names are never read. It is portable, so the mapping is tested on every OS; the Windows
// driver reads the two properties (mmdevice_windows.go).

// formFactorName is the name of an EndpointFormFactor value (mmdeviceapi.h), "unknown" beyond the enum.
func formFactorName(v uint32) string {
	names := []string{"RemoteNetworkDevice", "Speakers", "LineLevel", "Headphones", "Microphone", "Headset",
		"Handset", "UnknownDigitalPassthrough", "SPDIF", "DigitalAudioDisplayDevice", "UnknownFormFactor"}
	if int(v) < len(names) {
		return names[v]
	}
	return "unknown"
}

// busName maps a device's enumerator name (PKEY_Device_EnumeratorName: "USB", "HDAUDIO", "BTHENUM", …) to the
// report's bus: usb, hdaudio (internal and HDMI/DisplayPort audio; the form factor tells them apart), bluetooth,
// pci, virtual (software devices), or other. A vendor's own enumerator name never reaches the report.
func busName(enumerator string) string {
	switch e := strings.ToUpper(strings.TrimSpace(enumerator)); {
	case e == "":
		return "unknown"
	case e == "USB" || strings.HasPrefix(e, "USBAUDIO"):
		return "usb"
	case e == "HDAUDIO" || e == "INTELAUDIO" || e == "DISPLAY":
		return "hdaudio"
	case strings.HasPrefix(e, "BTH") || e == "BLUETOOTH":
		return "bluetooth"
	case e == "PCI":
		return "pci"
	case e == "SWD" || e == "ROOT" || e == "MMDEVAPI":
		return "virtual"
	default:
		return "other"
	}
}
