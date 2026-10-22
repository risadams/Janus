//go:build windows

package openai

import (
	"fmt"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// The Windows Credential Manager is read with CredReadW from advapi32,
// through the standard syscall package (ADR-005).
var (
	advapi32  = syscall.NewLazyDLL("advapi32.dll")
	credReadW = advapi32.NewProc("CredReadW")
	credFree  = advapi32.NewProc("CredFree")
)

// credential is the start of Windows' CREDENTIALW, as far as the blob.
type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        [8]byte
	CredentialBlobSize uint32
	CredentialBlob     *byte
}

const credTypeGeneric = 1

// readWindowsCredential reads the generic credential name. cmdkey stores
// the password as UTF-16, which is decoded; a blob that is not is taken as
// it is.
func readWindowsCredential(name, add string) (string, error) {
	target, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return "", fmt.Errorf("%q cannot name a credential", name)
	}
	var c *credential
	// #nosec G103 -- CredReadW takes the name and returns the credential through pointers, as its documentation says
	r, _, callErr := credReadW.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&c)))
	if r == 0 {
		return "", fmt.Errorf("the Windows Credential Manager has no generic credential %s (%v); add it with: %s", name, callErr, add)
	}
	defer credFree.Call(uintptr(unsafe.Pointer(c)))              // #nosec G103 G104 -- frees what CredReadW allocated; it returns nothing to check
	blob := unsafe.Slice(c.CredentialBlob, c.CredentialBlobSize) // #nosec G103 -- the blob and its size as CredReadW gave them
	if len(blob)%2 == 0 {
		u := make([]uint16, len(blob)/2)
		for i := range u {
			u[i] = uint16(blob[2*i]) | uint16(blob[2*i+1])<<8
		}
		return string(utf16.Decode(u)), nil
	}
	return string(blob), nil
}
