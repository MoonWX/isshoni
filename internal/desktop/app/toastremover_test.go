package app_test

import (
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/winos"
)

// The app finds the removal from Action Center only through a type assertion on Deps.OS (toasts.go), so nothing else
// would notice if winos.Hooks' RemoveToasts stopped matching toastRemover: the shipped app would then quietly stop
// removing stale toasts (03 §15; WF13-R3, WF14-R1). This assertion makes such a change a compile error. e2ehost's
// recorder asserts the same signature next to its other OSHooks checks (e2ehost/record.go).
var _ app.ToastRemover = (*winos.Hooks)(nil)
