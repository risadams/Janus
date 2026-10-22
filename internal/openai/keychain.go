package openai

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// The operating system's credential store is read with the tool the
// platform already has, so that janus needs no module to do it (ADR-005):
// security on macOS, secret-tool from libsecret on Linux and the BSDs, and
// CredReadW on Windows (keychain_windows.go). An entry is looked up by its
// name alone, the NAME in "keychain:NAME".

// keychainTool is the command that reads the entry name on goos, and the
// command a person runs to add it. ok is false where there is no tool.
func keychainTool(goos, name string) (argv []string, add string, ok bool) {
	switch goos {
	case "darwin":
		return []string{"security", "find-generic-password", "-s", name, "-w"},
			fmt.Sprintf("security add-generic-password -a \"$USER\" -s %s -w", name), true
	case "linux", "freebsd", "openbsd", "netbsd":
		return []string{"secret-tool", "lookup", "service", name},
			fmt.Sprintf("secret-tool store --label=%q service %s", "janus OpenAI key", name), true
	case "windows":
		return nil, fmt.Sprintf("cmdkey /generic:%s /user:janus /pass", name), true
	}
	return nil, "", false
}

// runTool runs a credential store's command and returns what it printed.
// It is replaced in tests.
var runTool = func(argv []string) (stdout []byte, stderr string, err error) {
	// #nosec G204 -- argv is one of keychainTool's fixed lists; name is
	// a single argument, never given to a shell
	cmd := exec.Command(argv[0], argv[1:]...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.Bytes(), strings.TrimSpace(errOut.String()), err
}

// readKeychain returns the secret stored as name in this machine's
// credential store. Its errors say how to add the entry, and never repeat
// anything the tool printed on its standard output, which on success is
// the key.
func readKeychain(name string) (string, error) {
	argv, add, ok := keychainTool(runtime.GOOS, name)
	switch {
	case !ok:
		return "", fmt.Errorf("janus cannot read a credential store on %s; use \"env:OPENAI_API_KEY\"", runtime.GOOS)
	case argv == nil:
		return readWindowsCredential(name, add)
	}
	out, stderr, err := runTool(argv)
	var exit *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "", fmt.Errorf("%s, which reads the credential store, is not installed; install it, or use \"env:OPENAI_API_KEY\"", argv[0])
	case errors.As(err, &exit):
		why := "it has no entry " + name
		if line, _, _ := strings.Cut(stderr, "\n"); line != "" {
			why += " (" + argv[0] + " said: " + line + ")"
		}
		return "", fmt.Errorf("%s; add it with: %s", why, add)
	case err != nil:
		return "", fmt.Errorf("%s could not be run: %v", argv[0], err)
	}
	return string(out), nil
}
