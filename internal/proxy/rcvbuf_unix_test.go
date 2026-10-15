//go:build unix

package proxy

import "syscall"

// fixReceiveBuffer sets a socket's receive buffer to n bytes, which stops
// the kernel sizing it for itself. The kernel may grant less.
func fixReceiveBuffer(fd uintptr, n int) {
	syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, n)
}
