package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/route"
)

// janus use talks to the running janus on its own listener.
func TestJanusUseMovesEverySessionAndClears(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &route.Router{Aliases: map[string]route.LocalModel{"janus-local": {ID: "qwen/qwen3.8-27b"}}}
	srv := &http.Server{Handler: withControl(http.NotFoundHandler(), r)}
	go srv.Serve(ln)
	defer srv.Close()
	dir := t.TempDir()
	data, _ := json.Marshal(map[string]any{"listen": ln.Addr().String(), "logging": map[string]any{"dir": filepath.Join(dir, "logs")}})
	path := filepath.Join(dir, "janus.config.json")
	os.WriteFile(path, data, 0o600)

	out, errOut, code := runCLI(t, "use", "-config", path, "local")
	if code != 0 || !strings.Contains(out, "every session: local") {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	out, _, _ = runCLI(t, "use", "-config", path, "-list")
	wantLines(t, out, "every session: local")
	out, _, _ = runCLI(t, "use", "-config", path, "-clear")
	wantLines(t, out, "No provider is chosen by hand; the routing rules decide.")
	if _, errOut, code := runCLI(t, "use", "-config", path, "codex"); code != 1 || !strings.Contains(errOut, "no openai section") {
		t.Errorf("codex with no openai section: exit %d, %s", code, errOut)
	}
}
