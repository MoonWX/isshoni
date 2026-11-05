package app

// ToastRemover exports toastRemover to the external tests (toastremover_test.go), so they can check at compile time
// that the real OS hooks provide it.
type ToastRemover = toastRemover
