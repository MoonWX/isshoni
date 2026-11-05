//go:build windows

package winos

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Action Center over WinRT's Windows.UI.Notifications (toasts.go has the why): ToastNotificationManager.GetDefault()
// (IToastNotificationManagerStatics5) gives the user's manager, whose History (IToastNotificationHistory and
// IToastNotificationHistory2) lists, removes and clears an AppUserModelID's toasts, and whose
// CreateToastNotifierWithId shows one. The interfaces are called through their vtables, as go-toast does for Wails'
// push: each slot below is the method's index after IInspectable's six (QueryInterface, AddRef, Release, GetIids,
// GetRuntimeClassName, GetTrustLevel), in the order of the Windows SDK's windows.ui.notifications.h and
// windows.data.xml.dom.h.

var (
	modCombase                    = windows.NewLazySystemDLL("combase.dll")
	procRoInitialize              = modCombase.NewProc("RoInitialize")
	procRoUninitialize            = modCombase.NewProc("RoUninitialize")
	procRoActivateInstance        = modCombase.NewProc("RoActivateInstance")
	procRoGetActivationFactory    = modCombase.NewProc("RoGetActivationFactory")
	procWindowsCreateString       = modCombase.NewProc("WindowsCreateString")
	procWindowsDeleteString       = modCombase.NewProc("WindowsDeleteString")
	procWindowsGetStringRawBuffer = modCombase.NewProc("WindowsGetStringRawBuffer")
)

// The runtime classes.
const (
	classToastManager      = "Windows.UI.Notifications.ToastNotificationManager"
	classToastNotification = "Windows.UI.Notifications.ToastNotification"
	classXMLDocument       = "Windows.Data.Xml.Dom.XmlDocument"
)

// The interfaces' IIDs.
var (
	iidToastManagerStatics5     = mustGUID("{D6F5F569-D40D-407C-8989-88CAB42CFD14}") // IToastNotificationManagerStatics5
	iidToastHistory2            = mustGUID("{3BC3D253-2F31-4092-9129-8AD5ABF067DA}") // IToastNotificationHistory2
	iidToastNotification2       = mustGUID("{9DFB9FD1-143A-490E-90BF-B9FBA7132DE7}") // IToastNotification2
	iidToastNotificationFactory = mustGUID("{04124B20-82C6-4229-B109-FD9ED4662B53}") // IToastNotificationFactory
	iidXMLDocument              = mustGUID("{F7F3A506-1E87-42D6-BCFB-B8C809FA5494}") // IXmlDocument
	iidXMLDocumentIO            = mustGUID("{6CD0E74E-EE65-4489-9EBF-CA43E87BA637}") // IXmlDocumentIO
	iidXMLNodeSerializer        = mustGUID("{5CC5B382-E6DD-4991-ABEF-06D8D2E7BD0C}") // IXmlNodeSerializer
)

// The vtable slots. IUnknown's: QueryInterface 0, Release 2.
const (
	slotQueryInterface = 0
	slotRelease        = 2

	slotStatics5GetDefault = 6 // IToastNotificationManagerStatics5.GetDefault

	slotForUserCreateNotifierWithID = 7 // IToastNotificationManagerForUser.CreateToastNotifierWithId
	slotForUserHistory              = 8 // IToastNotificationManagerForUser.get_History

	slotHistoryRemoveGroupedTagWithID = 8  // IToastNotificationHistory.RemoveGroupedTagWithId(tag, group, appId)
	slotHistoryClearWithID            = 12 // IToastNotificationHistory.ClearWithId(appId)
	slotHistory2GetHistoryWithID      = 7  // IToastNotificationHistory2.GetHistoryWithId(appId)

	slotVectorViewGetAt = 6 // IVectorView<ToastNotification>.GetAt
	slotVectorViewSize  = 7 // IVectorView<ToastNotification>.get_Size

	slotToastContent           = 6  // IToastNotification.get_Content
	slotToast2PutTag           = 6  // IToastNotification2.put_Tag
	slotToast2GetTag           = 7  // IToastNotification2.get_Tag
	slotToast2PutGroup         = 8  // IToastNotification2.put_Group
	slotToast2GetGroup         = 9  // IToastNotification2.get_Group
	slotToast2PutSuppressPopup = 10 // IToastNotification2.put_SuppressPopup

	slotFactoryCreateToast = 6 // IToastNotificationFactory.CreateToastNotification

	slotNotifierShow       = 6 // IToastNotifier.Show
	slotNotifierGetSetting = 8 // IToastNotifier.get_Setting

	slotXMLIOLoadXML     = 6 // IXmlDocumentIO.LoadXml
	slotXMLSerializerGet = 6 // IXmlNodeSerializer.GetXml
)

const (
	roInitMultithreaded = 1          // RO_INIT_MULTITHREADED
	sFalse              = 1          // S_FALSE: the thread was initialized already
	rpcEChangedMode     = 0x80010106 // RPC_E_CHANGED_MODE: an STA thread, where WinRT works too
	hrNotFound          = 0x80070490 // HRESULT_FROM_WIN32(ERROR_NOT_FOUND): no toasts of that app
)

func mustGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

// toastMu serializes this package's own calls into Action Center (ShowToast, DeliveredToasts, RemoveDeliveredToasts).
// It does not cover toasts shown any other way: the shell's toasts go out through Wails' notifications service
// (go-toast), which never takes it. A Wails toast delivered while RemoveDeliveredToasts clears the app's toasts can
// still be lost; RemoveToasts' doc (toasts.go) puts the ordering on its callers.
var toastMu sync.Mutex

// errNoWinRT is a Windows without WinRT's combase exports.
var errNoWinRT = errors.New("winos: WinRT is unavailable")

// hrError is a failed WinRT call.
func hrError(op string, hr uintptr) error {
	return fmt.Errorf("winos: %s: HRESULT %#x", op, uint32(hr)) //nolint:gosec // G115: an HRESULT is 32 bits
}

// failed is FAILED(hr).
func failed(hr uintptr) bool { return int32(uint32(hr)) < 0 } //nolint:gosec // G115: an HRESULT is 32 bits

// withWinRT runs fn on a locked thread in WinRT's multithreaded apartment (or the thread's own STA).
func withWinRT(fn func() error) error {
	for _, p := range []*windows.LazyProc{procRoInitialize, procRoUninitialize, procRoActivateInstance,
		procRoGetActivationFactory, procWindowsCreateString, procWindowsDeleteString, procWindowsGetStringRawBuffer} {
		if p.Find() != nil {
			return errNoWinRT
		}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := procRoInitialize.Call(roInitMultithreaded)
	switch {
	case hr == 0 || hr == sFalse:
		defer func() { _, _, _ = procRoUninitialize.Call() }()
	case uint32(hr) == rpcEChangedMode: //nolint:gosec // G115: an HRESULT is 32 bits
	default:
		return hrError("RoInitialize", hr)
	}
	return fn()
}

// hstring is an HSTRING; 0 is the empty string.
type hstring uintptr

func newHString(s string) (hstring, error) {
	if s == "" {
		return 0, nil
	}
	u := utf16.Encode([]rune(s))
	var h hstring
	hr, _, _ := procWindowsCreateString.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)),
		uintptr(unsafe.Pointer(&h)))
	if failed(hr) {
		return 0, hrError("WindowsCreateString", hr)
	}
	return h, nil
}

func (h hstring) free() {
	if h != 0 {
		_, _, _ = procWindowsDeleteString.Call(uintptr(h))
	}
}

// String copies the string out of h.
func (h hstring) String() string {
	if h == 0 {
		return ""
	}
	var n uint32
	p, _, _ := procWindowsGetStringRawBuffer.Call(uintptr(h), uintptr(unsafe.Pointer(&n)))
	if p == 0 || n == 0 {
		return ""
	}
	// p points into the HSTRING's own buffer, which isn't Go memory: read it through a pointer-typed view of p.
	buf := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	return string(utf16.Decode(unsafe.Slice((*uint16)(buf), n)))
}

// method is slot of obj's vtable.
func method(obj unsafe.Pointer, slot uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	return *(*uintptr)(unsafe.Add(vtbl, slot*unsafe.Sizeof(uintptr(0))))
}

func release(obj unsafe.Pointer) {
	if obj != nil {
		_, _, _ = syscall.SyscallN(method(obj, slotRelease), uintptr(obj))
	}
}

func queryInterface(obj unsafe.Pointer, iid *windows.GUID, what string) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	hr, _, _ := syscall.SyscallN(method(obj, slotQueryInterface), uintptr(obj), uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(&out)))
	if failed(hr) || out == nil {
		return nil, hrError("QueryInterface "+what, hr)
	}
	return out, nil
}

func activationFactory(class string, iid *windows.GUID) (unsafe.Pointer, error) {
	h, err := newHString(class)
	if err != nil {
		return nil, err
	}
	defer h.free()
	var out unsafe.Pointer
	hr, _, _ := procRoGetActivationFactory.Call(uintptr(h), uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if failed(hr) || out == nil {
		return nil, hrError("RoGetActivationFactory "+class, hr)
	}
	return out, nil
}

func activateInstance(class string) (unsafe.Pointer, error) {
	h, err := newHString(class)
	if err != nil {
		return nil, err
	}
	defer h.free()
	var out unsafe.Pointer
	hr, _, _ := procRoActivateInstance.Call(uintptr(h), uintptr(unsafe.Pointer(&out)))
	if failed(hr) || out == nil {
		return nil, hrError("RoActivateInstance "+class, hr)
	}
	return out, nil
}

// getObject calls a method whose only argument is an interface out-pointer.
func getObject(obj unsafe.Pointer, slot uintptr, op string) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	hr, _, _ := syscall.SyscallN(method(obj, slot), uintptr(obj), uintptr(unsafe.Pointer(&out)))
	if failed(hr) || out == nil {
		return nil, hrError(op, hr)
	}
	return out, nil
}

// getString calls a method whose only argument is an HSTRING out-pointer.
func getString(obj unsafe.Pointer, slot uintptr, op string) (string, error) {
	var h hstring
	hr, _, _ := syscall.SyscallN(method(obj, slot), uintptr(obj), uintptr(unsafe.Pointer(&h)))
	if failed(hr) {
		return "", hrError(op, hr)
	}
	defer h.free()
	return h.String(), nil
}

// putString calls a method whose only argument is an HSTRING.
func putString(obj unsafe.Pointer, slot uintptr, s, op string) error {
	h, err := newHString(s)
	if err != nil {
		return err
	}
	defer h.free()
	if hr, _, _ := syscall.SyscallN(method(obj, slot), uintptr(obj), uintptr(h)); failed(hr) {
		return hrError(op, hr)
	}
	return nil
}

// actionCenter is the user's toast manager and its history, for one AppUserModelID.
type actionCenter struct {
	app      hstring
	manager  unsafe.Pointer // IToastNotificationManagerForUser
	history  unsafe.Pointer // IToastNotificationHistory
	history2 unsafe.Pointer // IToastNotificationHistory2
}

func openActionCenter(appID string) (c *actionCenter, err error) {
	c = &actionCenter{}
	defer func() {
		if err != nil {
			c.close()
			c = nil
		}
	}()
	if c.app, err = newHString(appID); err != nil {
		return c, err
	}
	statics, err := activationFactory(classToastManager, &iidToastManagerStatics5)
	if err != nil {
		return c, err
	}
	defer release(statics)
	if c.manager, err = getObject(statics, slotStatics5GetDefault, "ToastNotificationManager.GetDefault"); err != nil {
		return c, err
	}
	if c.history, err = getObject(c.manager, slotForUserHistory, "get_History"); err != nil {
		return c, err
	}
	c.history2, err = queryInterface(c.history, &iidToastHistory2, "IToastNotificationHistory2")
	return c, err
}

func (c *actionCenter) close() {
	release(c.history2)
	release(c.history)
	release(c.manager)
	c.app.free()
}

// list is the app's toasts in Action Center (GetHistoryWithId).
func (c *actionCenter) list() ([]deliveredItem, error) {
	var view unsafe.Pointer
	hr, _, _ := syscall.SyscallN(method(c.history2, slotHistory2GetHistoryWithID), uintptr(c.history2), uintptr(c.app),
		uintptr(unsafe.Pointer(&view)))
	if uint32(hr) == hrNotFound { //nolint:gosec // G115: an HRESULT is 32 bits
		return nil, nil
	}
	if failed(hr) || view == nil {
		return nil, hrError("GetHistoryWithId", hr)
	}
	defer release(view)
	var n uint32
	if hr, _, _ := syscall.SyscallN(method(view, slotVectorViewSize), uintptr(view),
		uintptr(unsafe.Pointer(&n))); failed(hr) {
		return nil, hrError("get_Size", hr)
	}
	items := make([]deliveredItem, 0, n)
	for i := range n {
		var toast unsafe.Pointer
		hr, _, _ := syscall.SyscallN(method(view, slotVectorViewGetAt), uintptr(view), uintptr(i),
			uintptr(unsafe.Pointer(&toast)))
		if failed(hr) || toast == nil {
			return nil, hrError("GetAt", hr)
		}
		item, err := readToast(toast)
		release(toast)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// readToast reads a toast's tag, group and XML.
func readToast(toast unsafe.Pointer) (deliveredItem, error) {
	t2, err := queryInterface(toast, &iidToastNotification2, "IToastNotification2")
	if err != nil {
		return deliveredItem{}, err
	}
	defer release(t2)
	tag, err := getString(t2, slotToast2GetTag, "get_Tag")
	if err != nil {
		return deliveredItem{}, err
	}
	group, err := getString(t2, slotToast2GetGroup, "get_Group")
	if err != nil {
		return deliveredItem{}, err
	}
	content, err := getObject(toast, slotToastContent, "get_Content")
	if err != nil {
		return deliveredItem{}, err
	}
	defer release(content)
	ser, err := queryInterface(content, &iidXMLNodeSerializer, "IXmlNodeSerializer")
	if err != nil {
		return deliveredItem{}, err
	}
	defer release(ser)
	doc, err := getString(ser, slotXMLSerializerGet, "GetXml")
	if err != nil {
		return deliveredItem{}, err
	}
	return deliveredItem{DeliveredToast: deliveredToast(tag, group, doc), xml: doc}, nil
}

// remove removes the app's toast of tag and group (RemoveGroupedTagWithId).
func (c *actionCenter) remove(tag, group string) error {
	t, err := newHString(tag)
	if err != nil {
		return err
	}
	defer t.free()
	g, err := newHString(group)
	if err != nil {
		return err
	}
	defer g.free()
	if hr, _, _ := syscall.SyscallN(method(c.history, slotHistoryRemoveGroupedTagWithID), uintptr(c.history),
		uintptr(t), uintptr(g), uintptr(c.app)); failed(hr) {
		return hrError("RemoveGroupedTagWithId", hr)
	}
	return nil
}

// clear removes all the app's toasts (ClearWithId).
func (c *actionCenter) clear() error {
	if hr, _, _ := syscall.SyscallN(method(c.history, slotHistoryClearWithID), uintptr(c.history),
		uintptr(c.app)); failed(hr) {
		return hrError("ClearWithId", hr)
	}
	return nil
}

// notifier is the app's IToastNotifier.
func (c *actionCenter) notifier() (unsafe.Pointer, error) {
	var out unsafe.Pointer
	hr, _, _ := syscall.SyscallN(method(c.manager, slotForUserCreateNotifierWithID), uintptr(c.manager),
		uintptr(c.app), uintptr(unsafe.Pointer(&out)))
	if failed(hr) || out == nil {
		return nil, hrError("CreateToastNotifierWithId", hr)
	}
	return out, nil
}

// setting is the notifier's NotificationSetting: 0 Enabled, 1 DisabledForApplication, 2 DisabledForUser, 3
// DisabledByGroupPolicy, 4 DisabledByManifest.
func (c *actionCenter) setting() (int32, error) {
	n, err := c.notifier()
	if err != nil {
		return 0, err
	}
	defer release(n)
	var s int32
	if hr, _, _ := syscall.SyscallN(method(n, slotNotifierGetSetting), uintptr(n),
		uintptr(unsafe.Pointer(&s))); failed(hr) {
		return 0, hrError("get_Setting", hr)
	}
	return s, nil
}

// show delivers t.
func (c *actionCenter) show(t Toast) error {
	doc, err := activateInstance(classXMLDocument)
	if err != nil {
		return err
	}
	defer release(doc)
	io, err := queryInterface(doc, &iidXMLDocumentIO, "IXmlDocumentIO")
	if err != nil {
		return err
	}
	defer release(io)
	if err := putString(io, slotXMLIOLoadXML, t.XML, "LoadXml"); err != nil {
		return err
	}
	xdoc, err := queryInterface(doc, &iidXMLDocument, "IXmlDocument")
	if err != nil {
		return err
	}
	defer release(xdoc)
	factory, err := activationFactory(classToastNotification, &iidToastNotificationFactory)
	if err != nil {
		return err
	}
	defer release(factory)
	var toast unsafe.Pointer
	hr, _, _ := syscall.SyscallN(method(factory, slotFactoryCreateToast), uintptr(factory), uintptr(xdoc),
		uintptr(unsafe.Pointer(&toast)))
	if failed(hr) || toast == nil {
		return hrError("CreateToastNotification", hr)
	}
	defer release(toast)
	if t.Tag != "" || t.Group != "" || t.SuppressPopup {
		t2, err := queryInterface(toast, &iidToastNotification2, "IToastNotification2")
		if err != nil {
			return err
		}
		defer release(t2)
		if t.Tag != "" {
			if err := putString(t2, slotToast2PutTag, t.Tag, "put_Tag"); err != nil {
				return err
			}
		}
		if t.Group != "" {
			if err := putString(t2, slotToast2PutGroup, t.Group, "put_Group"); err != nil {
				return err
			}
		}
		if t.SuppressPopup {
			if hr, _, _ := syscall.SyscallN(method(t2, slotToast2PutSuppressPopup), uintptr(t2), 1); failed(hr) {
				return hrError("put_SuppressPopup", hr)
			}
		}
	}
	n, err := c.notifier()
	if err != nil {
		return err
	}
	defer release(n)
	if hr, _, _ := syscall.SyscallN(method(n, slotNotifierShow), uintptr(n), uintptr(toast)); failed(hr) {
		return hrError("Show", hr)
	}
	return nil
}

// withActionCenter runs fn with appID's Action Center, under toastMu and in WinRT.
func withActionCenter(appID string, fn func(c *actionCenter) error) error {
	toastMu.Lock()
	defer toastMu.Unlock()
	return withWinRT(func() error {
		c, err := openActionCenter(appID)
		if err != nil {
			return err
		}
		defer c.close()
		return fn(c)
	})
}

// DeliveredToasts lists appID's toasts in Action Center.
func DeliveredToasts(appID string) ([]DeliveredToast, error) {
	var out []DeliveredToast
	err := withActionCenter(appID, func(c *actionCenter) error {
		items, err := c.list()
		for _, it := range items {
			out = append(out, it.DeliveredToast)
		}
		return err
	})
	return out, err
}

// ShowToast delivers a toast for appID (with its tag and group, and without a banner when t.SuppressPopup).
func ShowToast(appID string, t Toast) error {
	return withActionCenter(appID, func(c *actionCenter) error { return c.show(t) })
}

// notificationSetting is appID's NotificationSetting (0: toasts are enabled).
func notificationSetting(appID string) (int32, error) {
	var s int32
	err := withActionCenter(appID, func(c *actionCenter) error {
		var err error
		s, err = c.setting()
		return err
	})
	return s, err
}

// RemoveDeliveredToasts removes appID's toasts that match from Action Center and reports how many went
// (removeDelivered, toasts.go, has the rules and the contract for callers).
func RemoveDeliveredToasts(appID string, match func(DeliveredToast) bool) (int, error) {
	removed := 0
	err := withActionCenter(appID, func(c *actionCenter) error {
		var err error
		removed, err = removeDelivered(c, match)
		return err
	})
	return removed, err
}

// removeToasts is RemoveDeliveredToasts for the Hooks.
func (*Hooks) removeToasts(appID string, match func(DeliveredToast) bool) (int, error) {
	return RemoveDeliveredToasts(appID, match)
}
