package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/openai/openaitest"
)

// testKey is made up, and distinctive enough that finding it in output can
// only mean it leaked.
const testKey = "janus-test-key-a6a488be5d85"

func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func loadTestKey(t *testing.T) Key {
	t.Helper()
	key, err := LoadKey(config.OpenAI{Key: "env:OPENAI_API_KEY"}, env(map[string]string{"OPENAI_API_KEY": testKey}))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestKeyIsReadFromTheNamedEnvironmentVariable(t *testing.T) {
	key, err := LoadKey(config.OpenAI{Key: "env:JANUS_OPENAI"}, env(map[string]string{
		"OPENAI_API_KEY": "sk-not-this-one",
		"JANUS_OPENAI":   "  " + testKey + "\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if key.secret != testKey {
		t.Errorf("read %q, want the JANUS_OPENAI value with its whitespace trimmed", key.secret)
	}
}

func TestKeyProblemsAreExplainedWithoutTheValue(t *testing.T) {
	for _, c := range []struct {
		name  string
		vars  map[string]string
		want  string
		value string
	}{
		{"unset", map[string]string{}, "OPENAI_API_KEY is not set", ""},
		{"empty", map[string]string{"OPENAI_API_KEY": "  "}, "OPENAI_API_KEY is empty", ""},
		{"two lines", map[string]string{"OPENAI_API_KEY": testKey + "\nX-Evil: 1"}, "a space or a character", "X-Evil"},
		{"space inside", map[string]string{"OPENAI_API_KEY": "Bearer " + testKey}, "a space or a character", testKey},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadKey(config.OpenAI{Key: "env:OPENAI_API_KEY"}, env(c.vars))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to say %q", err, c.want)
			}
			if c.value != "" && strings.Contains(err.Error(), c.value) {
				t.Errorf("the error repeats the value: %v", err)
			}
		})
	}
}

// fakeTool stands in for the credential store's command: it answers out,
// or fails with stderr when found is false.
func fakeTool(t *testing.T, out string, found bool, stderr string) *[]string {
	t.Helper()
	var ran []string
	saved := runTool
	runTool = func(argv []string) ([]byte, string, error) {
		ran = argv
		if !found {
			return []byte("partial-" + testKey), stderr, &exec.ExitError{}
		}
		return []byte(out), "", nil
	}
	t.Cleanup(func() { runTool = saved })
	return &ran
}

func TestTheKeyIsReadFromTheCredentialStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reads the Credential Manager directly; see keychain_windows.go")
	}
	ran := fakeTool(t, testKey+"\n", true, "")
	key, err := LoadKey(config.OpenAI{Key: "keychain:janus-openai"}, env(nil))
	if err != nil || key.secret != testKey {
		t.Fatalf("key %v, err %v", key.Set(), err)
	}
	if want, _, _ := keychainTool(runtime.GOOS, "janus-openai"); strings.Join(*ran, " ") != strings.Join(want, " ") {
		t.Errorf("ran %q, want %q", *ran, want)
	}
}

// A missing entry is explained with the command that adds it, and nothing
// the tool printed on its standard output is repeated.
func TestAMissingCredentialSaysHowToAddIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reads the Credential Manager directly")
	}
	fakeTool(t, "", false, "The specified item could not be found in the keychain.")
	_, err := LoadKey(config.OpenAI{Key: "keychain:janus-openai"}, env(nil))
	_, add, _ := keychainTool(runtime.GOOS, "janus-openai")
	if err == nil || !strings.Contains(err.Error(), "add it with: "+add) || !strings.Contains(err.Error(), "could not be found") {
		t.Errorf("err = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), testKey) {
		t.Errorf("the error repeats what the tool printed: %v", err)
	}
}

// Each platform's tool is run with a fixed argument list, the entry's name
// one argument of it, whatever the name holds.
func TestTheCredentialStoreIsReadWithAFixedCommand(t *testing.T) {
	name := "janus; rm -rf ~"
	for goos, want := range map[string][]string{
		"darwin": {"security", "find-generic-password", "-s", name, "-w"},
		"linux":  {"secret-tool", "lookup", "service", name},
	} {
		argv, add, ok := keychainTool(goos, name)
		if !ok || strings.Join(argv, "\x00") != strings.Join(want, "\x00") || add == "" {
			t.Errorf("%s: %q, add %q", goos, argv, add)
		}
	}
	if _, add, ok := keychainTool("windows", name); !ok || !strings.HasPrefix(add, "cmdkey /generic:") {
		t.Errorf("windows: add %q", add)
	}
	if _, _, ok := keychainTool("plan9", name); ok {
		t.Error("a store on plan9")
	}
}

// A key handed to a log line, a JSON record such as the ledger's, or any
// fmt verb prints as [redacted].
func TestKeyNeverPrintsItself(t *testing.T) {
	key := loadTestKey(t)
	var out bytes.Buffer
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		fmt.Fprintf(&out, verb+"\n", key)
		fmt.Fprintf(&out, verb+"\n", struct{ K Key }{key})
		fmt.Fprintf(&out, verb+"\n", &key)
	}
	j, _ := json.Marshal(map[string]any{"key": key, "ptr": &key})
	out.Write(j)
	slog.New(slog.NewJSONHandler(&out, nil)).Info("loaded", "key", key)
	slog.New(slog.NewTextHandler(&out, nil)).Info("loaded", "key", key, "in", struct{ K Key }{key})
	fmt.Fprint(&out, errors.New("wrapped: "+key.String()))

	if strings.Contains(out.String(), testKey) || strings.Contains(out.String(), fmt.Sprintf("%x", testKey)) {
		t.Fatalf("the key was printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), redacted) {
		t.Errorf("nothing printed as %s:\n%s", redacted, out.String())
	}
}

// The request to OpenAI carries the key and nothing of Claude Code's
// headers, so its Anthropic credential cannot cross.
func TestRequestCarriesOnlyTheOpenAIKey(t *testing.T) {
	srv := openaitest.New(t, openaitest.Answer(openaitest.Text{Text: "hi"}))
	srv.Key = testKey
	key := loadTestKey(t)

	req, err := NewRequest(context.Background(), key, srv.URL, "/v1/responses", []byte(`{"model":"gpt-6-luna","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the fake did not accept the key", resp.StatusCode)
	}

	got := srv.Requests()[0].Header
	for name := range got {
		switch strings.ToLower(name) {
		case "authorization", "content-type", "content-length", "user-agent", "accept-encoding":
		default:
			t.Errorf("request carries %s, which janus did not mean to send", name)
		}
	}
	if got.Get("X-Api-Key") != "" || got.Get("Anthropic-Version") != "" {
		t.Error("an Anthropic header reached OpenAI")
	}
}

func TestRequestNeedsAKeyAndAnHTTPBase(t *testing.T) {
	if _, err := NewRequest(context.Background(), Key{}, "https://api.openai.com", "/v1/responses", nil); err == nil {
		t.Error("a request was built with no key")
	}
	for _, base := range []string{"", "api.openai.com", "file:///etc", "ftp://api.openai.com"} {
		if _, err := NewRequest(context.Background(), loadTestKey(t), base, "/v1/responses", nil); err == nil {
			t.Errorf("a request was built for base %q", base)
		}
	}
}
