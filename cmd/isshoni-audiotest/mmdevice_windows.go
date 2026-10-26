//go:build windows

package main

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file lists the active render endpoints through the MMDevice API (docs/m2/05-windows-audio.md §18.4,
// §18.5 case 22, §18.7): each endpoint's id (only ever passed to `fixture endpoint-tone`, never reported), whether
// it is the default (eRender, eConsole), its form factor and bus (the report's {formFactor, bus}; friendly names
// are never read), and the default endpoint's master volume and mute (recorded, not enforced). The audiotest is a
// test tool, not the app: this COM work runs on a goroutine locked to its OS thread for the call, in the MTA, and
// releases every interface it gets.

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procPropVariantClear = ole32.NewProc("PropVariantClear")
)

// The COM ids (mmdeviceapi.h, endpointvolume.h, functiondiscoverykeys_devpkey.h).
var (
	clsidMMDeviceEnumerator = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C,
		Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35,
		Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioEndpointVolume = windows.GUID{Data1: 0x5CDF2C82, Data2: 0x841E, Data3: 0x4546,
		Data4: [8]byte{0x97, 0x22, 0x0C, 0xF7, 0x40, 0x78, 0x22, 0x9A}}
	pkeyAudioEndpointFormFactor = propertyKey{fmtid: windows.GUID{Data1: 0x1DA5D803, Data2: 0xD492, Data3: 0x4EDD,
		Data4: [8]byte{0x8C, 0x23, 0xE0, 0xC0, 0xFF, 0xEE, 0x7F, 0x0E}}, pid: 0}
	pkeyDeviceEnumeratorName = propertyKey{fmtid: windows.GUID{Data1: 0xA45C254E, Data2: 0xDF1C, Data3: 0x4EFD,
		Data4: [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}}, pid: 24}
)

// The constants of the calls below.
const (
	eRender           = 0
	eConsole          = 0
	deviceStateActive = 1
	stgmRead          = 0
	clsctxAll         = 0x17
	vtUI4             = 19
	vtLPWSTR          = 31
	rpcEChangedMode   = 0x80010106
)

// The vtable slots used (IUnknown is 0–2 in each).
const (
	slotRelease                 = 2
	slotEnumAudioEndpoints      = 3 // IMMDeviceEnumerator
	slotGetDefaultAudioEndpoint = 4
	slotCollectionGetCount      = 3 // IMMDeviceCollection
	slotCollectionItem          = 4
	slotDeviceActivate          = 3 // IMMDevice
	slotDeviceOpenPropertyStore = 4
	slotDeviceGetID             = 5
	slotPropertyStoreGetValue   = 5 // IPropertyStore
	slotGetMasterVolumeScalar   = 9 // IAudioEndpointVolume
	slotGetMute                 = 15
)

// propertyKey is PROPERTYKEY.
type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

// propVariant is PROPVARIANT on 64-bit Windows: the type, three reserved words, and a 16-byte union whose first
// eight bytes hold a VT_UI4's value or a VT_LPWSTR's pointer. The union is plain words, never a Go pointer field: a
// VT_UI4 value of 0–10 in a pointer field would be an invalid pointer to the garbage collector.
type propVariant struct {
	vt  uint16
	_   [3]uint16
	val [2]uint64
}

// comObject is a COM interface pointer: its first field points at the vtable.
type comObject struct {
	vtbl *[32]uintptr
}

// call invokes vtable slot with this object first. uintptrescapes keeps pointer arguments converted at the call
// site alive and in place for the call, as for syscall.SyscallN.
//
//go:uintptrescapes
func (o *comObject) call(slot int, args ...uintptr) int32 {
	r, _, _ := syscall.SyscallN(o.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return int32(r) //nolint:gosec // G115: an HRESULT is the low 32 bits
}

func (o *comObject) release() {
	if o != nil {
		o.call(slotRelease)
	}
}

// hresultError is a failed HRESULT.
func hresultError(op string, hr int32) error {
	return fmt.Errorf("%s: HRESULT 0x%08X", op, uint32(hr)) //nolint:gosec // G115: an HRESULT's bits
}

// endpointDesc is one active render endpoint.
type endpointDesc struct {
	id         string // the IMMDevice id: for `fixture endpoint-tone` only, never reported
	isDefault  bool
	formFactor string
	bus        string
}

// endpointList is the active render endpoints, the default first, and the default's volume and mute (nil when
// unreadable).
type endpointList struct {
	endpoints []endpointDesc
	volume    *float64
	muted     *bool
}

// listRenderEndpoints enumerates the active render endpoints (eRender, DEVICE_STATE_ACTIVE).
func listRenderEndpoints() (endpointList, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	switch err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); {
	case err == nil, errors.Is(err, syscall.Errno(1)): // S_OK, S_FALSE: balanced by CoUninitialize
		defer windows.CoUninitialize()
	case errors.Is(err, syscall.Errno(rpcEChangedMode)): // the thread is an STA already: COM works, nothing to undo
	default:
		return endpointList{}, fmt.Errorf("CoInitializeEx: %w", err)
	}

	var enum *comObject
	if r, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum))); int32(r) < 0 { //nolint:gosec // G115: an HRESULT
		return endpointList{}, hresultError("CoCreateInstance(MMDeviceEnumerator)", int32(r)) //nolint:gosec // G115
	}
	defer enum.release()

	var out endpointList
	defaultID := ""
	var def *comObject
	if hr := enum.call(slotGetDefaultAudioEndpoint, eRender, eConsole, uintptr(unsafe.Pointer(&def))); hr >= 0 {
		defaultID, _ = deviceID(def)
		out.volume, out.muted = endpointVolume(def)
		def.release()
	}

	var coll *comObject
	if hr := enum.call(slotEnumAudioEndpoints, eRender, deviceStateActive, uintptr(unsafe.Pointer(&coll))); hr < 0 {
		return endpointList{}, hresultError("EnumAudioEndpoints", hr)
	}
	defer coll.release()
	var n uint32
	if hr := coll.call(slotCollectionGetCount, uintptr(unsafe.Pointer(&n))); hr < 0 {
		return endpointList{}, hresultError("IMMDeviceCollection::GetCount", hr)
	}
	for i := range n {
		var dev *comObject
		if hr := coll.call(slotCollectionItem, uintptr(i), uintptr(unsafe.Pointer(&dev))); hr < 0 {
			return endpointList{}, hresultError("IMMDeviceCollection::Item", hr)
		}
		e, err := describeEndpoint(dev)
		dev.release()
		if err != nil {
			return endpointList{}, err
		}
		e.isDefault = e.id == defaultID
		if e.isDefault {
			out.endpoints = append([]endpointDesc{e}, out.endpoints...)
		} else {
			out.endpoints = append(out.endpoints, e)
		}
	}
	return out, nil
}

// deviceID is IMMDevice::GetId.
func deviceID(dev *comObject) (string, error) {
	var p *uint16
	if hr := dev.call(slotDeviceGetID, uintptr(unsafe.Pointer(&p))); hr < 0 {
		return "", hresultError("IMMDevice::GetId", hr)
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(p))
	return windows.UTF16PtrToString(p), nil
}

// describeEndpoint reads an endpoint's id, form factor and bus.
func describeEndpoint(dev *comObject) (endpointDesc, error) {
	id, err := deviceID(dev)
	if err != nil {
		return endpointDesc{}, err
	}
	e := endpointDesc{id: id, formFactor: "unknown", bus: "unknown"}
	var store *comObject
	if hr := dev.call(slotDeviceOpenPropertyStore, stgmRead, uintptr(unsafe.Pointer(&store))); hr < 0 {
		return e, nil // the properties are recorded, not needed: keep "unknown"
	}
	defer store.release()
	if v, ok := propUI4(store, &pkeyAudioEndpointFormFactor); ok {
		e.formFactor = formFactorName(v)
	}
	if s, ok := propString(store, &pkeyDeviceEnumeratorName); ok {
		e.bus = busName(s)
	}
	return e, nil
}

// propUI4 reads a VT_UI4 property.
func propUI4(store *comObject, key *propertyKey) (uint32, bool) {
	var pv propVariant
	if hr := store.call(slotPropertyStoreGetValue, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&pv))); hr < 0 {
		return 0, false
	}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv))) //nolint:errcheck // nothing to do on failure
	if pv.vt != vtUI4 {
		return 0, false
	}
	return uint32(pv.val[0]), true //nolint:gosec // G115: a VT_UI4 is the union's low 32 bits
}

// propString reads a VT_LPWSTR property.
func propString(store *comObject, key *propertyKey) (string, bool) {
	var pv propVariant
	if hr := store.call(slotPropertyStoreGetValue, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&pv))); hr < 0 {
		return "", false
	}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv))) //nolint:errcheck // nothing to do on failure
	if pv.vt != vtLPWSTR || pv.val[0] == 0 {
		return "", false
	}
	return windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&pv.val[0]))), true
}

// endpointVolume reads the endpoint's master volume (0..1) and mute through IAudioEndpointVolume; nil when it can't.
func endpointVolume(dev *comObject) (*float64, *bool) {
	var vol *comObject
	if hr := dev.call(slotDeviceActivate, uintptr(unsafe.Pointer(&iidIAudioEndpointVolume)), clsctxAll, 0,
		uintptr(unsafe.Pointer(&vol))); hr < 0 {
		return nil, nil
	}
	defer vol.release()
	var level float32
	var mute int32
	var lp *float64
	var mp *bool
	if hr := vol.call(slotGetMasterVolumeScalar, uintptr(unsafe.Pointer(&level))); hr >= 0 {
		v := math.Round(float64(level)*100) / 100
		lp = &v
	}
	if hr := vol.call(slotGetMute, uintptr(unsafe.Pointer(&mute))); hr >= 0 {
		m := mute != 0
		mp = &m
	}
	return lp, mp
}
