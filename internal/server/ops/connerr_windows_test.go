//go:build windows

package ops

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// Winsock's own numbers, as connect and recv return them: WSAECONNREFUSED for a socket file nobody listens on,
// WSAECONNRESET and WSAECONNABORTED for a connection the other side closed. The POSIX names of package syscall
// are other, invented values on Windows.
func TestWinsockErrnos(t *testing.T) {
	if err := os.NewSyscallError("connectex", syscall.Errno(10061)); !isConnRefused(err) {
		t.Errorf("WSAECONNREFUSED (10061) is not a refused dial")
	}
	for _, e := range []syscall.Errno{10054, 10053} {
		if err := os.NewSyscallError("wsarecv", e); !isConnReset(err) || !isHangup(err) {
			t.Errorf("errno %d is not a closed connection", e)
		}
	}
	if errors.Is(syscall.Errno(10061), syscall.ECONNREFUSED) || errors.Is(syscall.Errno(10054), syscall.ECONNRESET) {
		t.Error("Winsock's numbers match the invented POSIX ones after all: connerr_windows.go can go")
	}
}
