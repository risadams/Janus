//go:build !unix

package proxy

// fixReceiveBuffer does nothing here: the stall it prevents was seen only
// on Linux (docs/research/loopback-stall.md).
func fixReceiveBuffer(fd uintptr, n int) {}
