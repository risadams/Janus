//go:build !windows

package openai

import "errors"

// readWindowsCredential is only reached on Windows.
func readWindowsCredential(name, add string) (string, error) {
	return "", errors.New("the Windows credential store is read only on Windows")
}
