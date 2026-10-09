//go:build !linux

package doctor

// adjtimex is Linux's; elsewhere the clock's sync state is unknown.
var adjtimex func() (unsynced bool, err error)
