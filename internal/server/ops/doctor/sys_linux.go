//go:build linux

package doctor

import "golang.org/x/sys/unix"

// adjtimex reads the kernel's clock state without changing it (an empty request) and reports whether the kernel
// considers the clock unsynchronized (STA_UNSYNC): no NTP or PTP daemon is steering it. The error is EPERM or
// ENOSYS where the call is filtered: systemd's ProtectClock= and SystemCallFilter=, or a container's seccomp
// profile (04 §13.2 clock).
var adjtimex = func() (unsynced bool, err error) {
	var tx unix.Timex
	if _, err := unix.Adjtimex(&tx); err != nil {
		return false, err
	}
	return tx.Status&unix.STA_UNSYNC != 0, nil
}
