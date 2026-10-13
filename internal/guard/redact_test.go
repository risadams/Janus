package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/risadams/janus/internal/anthropic"
)

func body(t *testing.T, system string, messages ...map[string]any) []byte {
	t.Helper()
	return request(t, system, messages...).Bytes()
}

func redact(t *testing.T, r *Redactor, b []byte) *Redaction {
	t.Helper()
	rd, err := r.Redact(b)
	if err != nil {
		t.Fatal(err)
	}
	return rd
}

func TestRedactReplacesSecretsEverywhereAndNothingElse(t *testing.T) {
	ps := positives()
	awsID, pem, dbPass, github := ps[3].want, ps[13].want, ps[14].want, ps[7].want
	b := body(t, "You are an agent.",
		user(textBlock("deploy with key "+awsID)),
		assistant(
			// The model repeated the password with nothing around it to
			// mark it as one, and used the token in a command.
			map[string]any{"type": "thinking", "thinking": "the password is " + dbPass, "signature": "c2ln"},
			textBlock("I will use "+dbPass+" to connect."),
			map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "git clone https://" + github + "@github.com/acme/app.git"}},
		),
		user(map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": ps[14].text + "\n" + ps[13].text}),
	)

	r := NewRedactor(&Detector{})
	rd := redact(t, r, b)

	for name, secret := range map[string]string{"aws key": awsID, "password": dbPass, "github token": github, "private key": pem, "private key, escaped": strings.ReplaceAll(pem, "\n", `\n`)} {
		if bytes.Contains(rd.Body, []byte(secret)) {
			t.Errorf("the %s is still in the request", name)
		}
	}
	if rd.Replaced != 4 || !rd.Active() {
		t.Errorf("Replaced = %d, want 4", rd.Replaced)
	}

	// It is still a valid request, the same shape as before, and everything
	// that was not a secret is untouched.
	out, err := anthropic.ParseRequest(rd.Body)
	if err != nil {
		t.Fatalf("the redacted request does not parse: %v", err)
	}
	if len(out.Messages()) != 3 || out.Model() != "claude-opus-5-5" {
		t.Errorf("shape changed: %d messages, model %q", len(out.Messages()), out.Model())
	}
	for _, kept := range []string{"You are an agent.", "deploy with key ", "I will use ", " to connect.", "git clone https://", "@github.com/acme/app.git", "postgres://app:", "@db.internal:5432/orders", `"signature":"c2ln"`, `"tool_use_id":"t1"`} {
		if !bytes.Contains(rd.Body, []byte(kept)) {
			t.Errorf("lost %q", kept)
		}
	}
	// Each secret became one placeholder, used at every place it appeared.
	if n := len(placeholderLike.FindAll(rd.Body, -1)); n != 6 {
		t.Errorf("%d placeholders in the body, want 6 (the password three times)", n)
	}
	if rd.Report.Findings == 0 || rd.Report.Kinds["aws_access_key"] != 1 {
		t.Errorf("report = %+v", rd.Report)
	}
}

func TestPlaceholdersAreStableSoTheCacheHolds(t *testing.T) {
	r := NewRedactor(&Detector{})
	key := positives()[3]
	turn1 := body(t, "sys", user(textBlock(key.text)))
	turn2 := body(t, "sys", user(textBlock(key.text)), assistant(textBlock("ok")), user(textBlock("and again")))

	a, b := redact(t, r, turn1), redact(t, r, turn2)
	tokenA, tokenB := placeholderLike.Find(a.Body), placeholderLike.Find(b.Body)
	if tokenA == nil || !bytes.Equal(tokenA, tokenB) {
		t.Fatalf("placeholders differ between turns: %s and %s", tokenA, tokenB)
	}
	// The second turn's request begins with the first's, byte for byte, up
	// to where the conversation grew: that is what a cache prefix needs.
	prefix := a.Body[:bytes.Index(a.Body, tokenA)+len(tokenA)]
	if !bytes.HasPrefix(b.Body, prefix) {
		t.Error("the redacted prefix changed between turns")
	}
	// Redacting the same request twice gives the same bytes.
	if again := redact(t, r, turn1); !bytes.Equal(again.Body, a.Body) {
		t.Error("redacting the same request twice gave different results")
	}
	// A different janus gives different placeholders, so one cannot be
	// worked back to its secret by anyone who sees it.
	if other := redact(t, NewRedactor(&Detector{}), turn1); bytes.Equal(placeholderLike.Find(other.Body), tokenA) {
		t.Error("two redactors produced the same placeholder for a secret")
	}
}

// Once janus has seen a secret, it is removed wherever it appears later,
// even where nothing marks it as a secret and the place it was first seen
// is gone from the conversation.
// A restarted proxy loads the same key, so a secret keeps its placeholder
// and the cache written before the restart still holds.
func TestPlaceholdersSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", KeyFile)
	key, err := LoadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	turn := body(t, "sys", user(textBlock(positives()[3].text)))
	before := placeholderLike.Find(redact(t, NewRedactorWithKey(&Detector{}, key), turn).Body)

	again, err := LoadKey(path)
	if err != nil || !bytes.Equal(again, key) {
		t.Fatalf("the key changed on reload: %v", err)
	}
	after := placeholderLike.Find(redact(t, NewRedactorWithKey(&Detector{}, again), turn).Body)
	if before == nil || !bytes.Equal(before, after) {
		t.Errorf("placeholder %s before the restart, %s after", before, after)
	}

	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("key file mode %v, want 0600", info.Mode().Perm())
		}
	}
}

// A file where the key should be that is not one is reported, never
// replaced: a new key would change every placeholder already sent.
func TestAKeyFileThatIsNotAKeyIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeyFile)
	os.WriteFile(path, []byte("not a key"), 0o600)
	if _, err := LoadKey(path); err == nil || !strings.Contains(err.Error(), "not a 32-byte placeholder key") {
		t.Errorf("LoadKey = %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "not a key" {
		t.Error("the file was replaced")
	}
}

func TestKnownSecretsAreRemovedWithoutContext(t *testing.T) {
	r := NewRedactor(&Detector{})
	pass := positives()[14]
	redact(t, r, body(t, "sys", user(textBlock(pass.text))))

	later := body(t, "sys", user(textBlock("earlier you told me the value was "+pass.want+", remember?")))
	if n := len((&Detector{}).Scan("the value was " + pass.want + ", remember?")); n != 0 {
		t.Fatal("test is wrong: the detector should not find this without context")
	}
	rd := redact(t, r, later)
	if bytes.Contains(rd.Body, []byte(pass.want)) || rd.Replaced != 1 {
		t.Errorf("a secret seen earlier was sent in a later request: replaced %d", rd.Replaced)
	}
}

func TestRequestWithoutSecretsIsUntouched(t *testing.T) {
	b := body(t, "sys", user(textBlock("count the lines in a.txt")))
	rd := redact(t, NewRedactor(&Detector{}), b)
	if rd.Active() || rd.Replaced != 0 || &rd.Body[0] != &b[0] {
		t.Errorf("a clean request was copied or changed: active=%v replaced=%d", rd.Active(), rd.Replaced)
	}
}

func TestRedactFailsClosed(t *testing.T) {
	r := NewRedactor(&Detector{})
	if _, err := r.Redact([]byte(`{"model":`)); err == nil {
		t.Error("an unreadable request was passed as safe")
	}
	if _, err := r.Redact([]byte(`["not","a","request"]`)); err == nil {
		t.Error("something that is not a request was passed as safe")
	}
}

// Secrets with characters JSON escapes, in each way a client may have
// escaped them.
func TestRedactHandlesEscaping(t *testing.T) {
	tricky := j("p@ss", `"w0rd`, `\with`, "<tags>&", "9Kq")
	text := `password = '` + tricky + `'`
	// A value this awkward fits no built-in format, so a custom pattern
	// finds it; what is under test is the replacing, not the finding.
	d := &Detector{Patterns: []*regexp.Regexp{regexp.MustCompile(`password = '([^']+)'`)}}
	if found := d.Scan(text); len(found) != 1 || found[0].Value(text) != tricky {
		t.Fatalf("test is wrong: detector found %+v", found)
	}

	goStyle, _ := json.Marshal(text) // escapes < > & as < and so on
	var jsStyle bytes.Buffer
	enc := json.NewEncoder(&jsStyle)
	enc.SetEscapeHTML(false)
	enc.Encode(text)

	for name, encoded := range map[string]string{"as Go writes it": string(goStyle), "as JavaScript writes it": strings.TrimSpace(jsStyle.String())} {
		b := []byte(`{"model":"m","messages":[{"role":"user","content":` + encoded + `}]}`)
		rd := redact(t, NewRedactor(d), b)
		req, err := anthropic.ParseRequest(rd.Body)
		if err != nil {
			t.Fatalf("%s: redacted request does not parse: %v", name, err)
		}
		got := req.Messages()[0].Blocks()[0].Text()
		if strings.Contains(got, tricky) || strings.Contains(got, "w0rd") || !placeholderLike.MatchString(got) {
			t.Errorf("%s: after redaction the message reads %q", name, got)
		}
		if !strings.HasPrefix(got, "password = '") || !strings.HasSuffix(got, "'") {
			t.Errorf("%s: the text around the secret changed: %q", name, got)
		}
	}
}

func TestLongerSecretsAreReplacedWhole(t *testing.T) {
	r := NewRedactor(&Detector{Patterns: nil})
	short := j("Kq7", "Rz2", "Lp9", "Xv") // a secret that is also the start of another
	long := short + j("B4n", "Kd8", "Yt3")
	b := body(t, "sys", user(textBlock("token = "+short+"\napi_key = "+long)))
	rd := redact(t, r, b)
	if rd.Replaced != 2 {
		t.Fatalf("Replaced = %d, want 2", rd.Replaced)
	}
	text := mustText(t, rd.Body)
	if strings.Contains(text, short) || len(placeholderLike.FindAllString(text, -1)) != 2 {
		t.Errorf("after redaction: %q", text)
	}
	// Neither placeholder is followed by a leftover piece of a secret.
	if strings.Contains(text, "B4n") {
		t.Errorf("the longer secret was only partly replaced: %q", text)
	}
}

func mustText(t *testing.T, b []byte) string {
	t.Helper()
	req, err := anthropic.ParseRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	return req.Messages()[0].Blocks()[0].Text()
}

func TestAllowedValuesAreNotRedacted(t *testing.T) {
	key := positives()[3]
	rd := redact(t, NewRedactor(&Detector{Allow: []string{key.want}}), body(t, "sys", user(textBlock(key.text))))
	if rd.Active() || !bytes.Contains(rd.Body, []byte(key.want)) {
		t.Error("an allowed value was redacted")
	}
}

func TestRestoreJSON(t *testing.T) {
	r := NewRedactor(&Detector{})
	ps := positives()
	pem, github := ps[13], ps[7]
	rd := redact(t, r, body(t, "sys", user(textBlock(pem.text+"\n"+github.text))))
	keyToken, ghToken := r.placeholder(pem.want), r.placeholder(github.want)

	response := `{"type":"message","content":[{"type":"text","text":"Using ` + ghToken + ` now."},` +
		`{"type":"tool_use","id":"t","name":"Write","input":{"path":"id_rsa","content":"` + keyToken + `"}}],"stop_reason":"tool_use"}`
	out, restored, err := rd.RestoreJSON([]byte(response))
	if err != nil || restored != 2 {
		t.Fatalf("restored %d, err %v", restored, err)
	}
	var msg struct {
		Content []struct {
			Text  string
			Input struct{ Path, Content string }
		}
	}
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("the restored response is not valid JSON: %v\n%s", err, out)
	}
	// The key has newlines in it; after restoring, the tool call's argument
	// must be the key exactly, which only works if it was escaped properly.
	if msg.Content[0].Text != "Using "+github.want+" now." || msg.Content[1].Input.Content != pem.want || msg.Content[1].Input.Path != "id_rsa" {
		t.Errorf("restored content = %+v", msg.Content)
	}
}

func TestRestoreRefusesWhatItCannotResolve(t *testing.T) {
	r := NewRedactor(&Detector{})
	key := positives()[3]
	rd := redact(t, r, body(t, "sys", user(textBlock(key.text))))
	good := r.placeholder(key.want)

	cases := map[string]string{
		"a placeholder janus never issued": placeholderPrefix + "0123456789ab",
		"a cut-off placeholder":            good[:len(good)-4],
		"the prefix alone":                 placeholderPrefix,
		"the wrong case":                   strings.ToLower(placeholderPrefix) + "0123456789ab",
		"letters where the id should be":   placeholderPrefix + "zzzzzzzzzzzz",
	}
	for name, token := range cases {
		_, _, err := rd.RestoreJSON([]byte(`{"type":"message","content":[{"type":"text","text":"use ` + token + ` here"}]}`))
		if !errors.Is(err, ErrUnresolvable) {
			t.Errorf("%s: err = %v, want ErrUnresolvable", name, err)
		}
	}
	// A response with no placeholders, and one that only mentions janus.
	for _, fine := range []string{`{"type":"message","content":[{"type":"text","text":"all done"}]}`, `{"type":"message","content":[{"type":"text","text":"JANUS is the proxy; REDACTED means hidden"}]}`} {
		if _, n, err := rd.RestoreJSON([]byte(fine)); err != nil || n != 0 {
			t.Errorf("%s: restored %d, err %v", fine, n, err)
		}
	}
}

// A request that itself contains placeholder-shaped text, such as a
// document about janus, must not make the model's repeating it an error.
func TestPlaceholderShapedTextFromTheClientIsNotAnError(t *testing.T) {
	r := NewRedactor(&Detector{})
	doc := placeholderPrefix + "aaaaaaaaaaaa"
	key := positives()[3]
	rd := redact(t, r, body(t, "sys", user(textBlock("the docs say a placeholder looks like "+doc+"; my key is "+key.want))))

	out, restored, err := rd.RestoreJSON([]byte(`{"type":"message","content":[{"type":"text","text":"Yes, like ` + doc + `, and yours is ` + r.placeholder(key.want) + `."}]}`))
	if err != nil || restored != 1 {
		t.Fatalf("restored %d, err %v", restored, err)
	}
	if !bytes.Contains(out, []byte(doc)) || !bytes.Contains(out, []byte(key.want)) {
		t.Errorf("out = %s", out)
	}
}

// sse builds a stream of text deltas for block 0, one event per piece.
func sse(deltaType, field string, pieces ...string) string {
	var b strings.Builder
	b.WriteString("event: message_start\n" + `data: {"type":"message_start","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":2}}}` + "\n\n")
	b.WriteString("event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
	for _, p := range pieces {
		enc, _ := json.Marshal(p)
		fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":%q,%q:%s}}\n\n", deltaType, field, enc)
	}
	b.WriteString("event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n")
	b.WriteString("event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}` + "\n\n")
	b.WriteString("event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n")
	return b.String()
}

// joined reads a stream and returns the text of block 0 put back together,
// and whether an error event was seen.
func joined(t *testing.T, stream string) (text string, sawError bool) {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string
			Delta struct {
				Text, Thinking string
				PartialJSON    string `json:"partial_json"`
			}
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("an event in the output is not valid JSON: %q", data)
		}
		if ev.Type == "error" {
			sawError = true
		}
		text += ev.Delta.Text + ev.Delta.Thinking + ev.Delta.PartialJSON
	}
	return text, sawError
}

func restoreStream(t *testing.T, rd *Redaction, stream string, oneByteAtATime bool) (string, *Restorer) {
	t.Helper()
	var src io.Reader = strings.NewReader(stream)
	if oneByteAtATime {
		src = iotest.OneByteReader(src)
	}
	r := rd.Restore(io.NopCloser(src))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), r
}

// The heart of it: a placeholder cut at every possible point between two
// pieces of a stream is still restored, and nothing else changes.
func TestStreamRestoresPlaceholdersCutAnywhere(t *testing.T) {
	r := NewRedactor(&Detector{})
	key := positives()[3]
	rd := redact(t, r, body(t, "sys", user(textBlock(key.text))))
	token := r.placeholder(key.want)
	full := "Use " + token + " for the deploy, then " + token + " again."
	want := strings.ReplaceAll(full, token, key.want)

	for cut := 0; cut <= len(full); cut++ {
		for _, bytewise := range []bool{false, true} {
			out, restorer := restoreStream(t, rd, sse("text_delta", "text", full[:cut], full[cut:]), bytewise)
			got, sawError := joined(t, out)
			if got != want || sawError || restorer.Err != nil || restorer.Restored != 2 {
				t.Fatalf("cut at %d (bytewise %v): got %q, restored %d, err %v", cut, bytewise, got, restorer.Restored, restorer.Err)
			}
			if strings.Contains(out, placeholderPrefix) {
				t.Fatalf("cut at %d: a placeholder reached the client: %s", cut, out)
			}
		}
	}
	// And cut into three, with the middle piece wholly inside the placeholder.
	at := strings.Index(full, token)
	out, _ := restoreStream(t, rd, sse("text_delta", "text", full[:at+5], full[at+5:at+15], full[at+15:]), false)
	if got, _ := joined(t, out); got != want {
		t.Errorf("three-way cut: got %q", got)
	}
}

func TestStreamRestoresInToolArgumentsAndThinking(t *testing.T) {
	r := NewRedactor(&Detector{})
	pem := positives()[13]
	rd := redact(t, r, body(t, "sys", user(textBlock(pem.text))))
	token := r.placeholder(pem.want)

	// A tool call writing the key to a file. The arguments arrive as pieces
	// of JSON text; put back together and parsed, the content must be the
	// key exactly, newlines and all.
	args := `{"path":"id_rsa","content":"` + token + `"}`
	for cut := 0; cut <= len(args); cut++ {
		out, restorer := restoreStream(t, rd, sse("input_json_delta", "partial_json", args[:cut], args[cut:]), false)
		assembled, sawError := joined(t, out)
		var parsed struct{ Path, Content string }
		if err := json.Unmarshal([]byte(assembled), &parsed); err != nil || sawError {
			t.Fatalf("cut at %d: the tool arguments are not valid JSON after restoring: %v\n%s", cut, err, assembled)
		}
		if parsed.Content != pem.want || parsed.Path != "id_rsa" || restorer.Restored != 1 {
			t.Fatalf("cut at %d: content matches %v, restored %d", cut, parsed.Content == pem.want, restorer.Restored)
		}
	}

	out, _ := restoreStream(t, rd, sse("thinking_delta", "thinking", "the key is "+token[:9], token[9:]+" so"), false)
	if got, _ := joined(t, out); got != "the key is "+pem.want+" so" {
		t.Errorf("thinking: got %q", got)
	}
}

func TestStreamWithNothingToRestoreIsByteIdentical(t *testing.T) {
	r := NewRedactor(&Detector{})
	rd := redact(t, r, body(t, "sys", user(textBlock(positives()[3].text))))

	streams := map[string]string{
		"ordinary text":     sse("text_delta", "text", "Hello, ", "the build passed. ", "Just checking."),
		"crlf line ends":    strings.ReplaceAll(sse("text_delta", "text", "Hello"), "\n", "\r\n"),
		"a signature":       strings.Replace(sse("text_delta", "text", "x"), `{"type":"text_delta","text":"x"}`, `{"type":"signature_delta","signature":"c2lnbmF0dXJl"}`, 1),
		"cut off mid-event": sse("text_delta", "text", "Hello")[:150],
		"empty":             "",
	}
	for name, stream := range streams {
		for _, bytewise := range []bool{false, true} {
			out, restorer := restoreStream(t, rd, stream, bytewise)
			if out != stream || restorer.Restored != 0 || restorer.Err != nil {
				t.Errorf("%s (bytewise %v): the stream was changed\n got %q\nwant %q", name, bytewise, out, stream)
			}
		}
	}
}

// Text that merely starts like a placeholder is held back only until it is
// clear it is not one, and then delivered unchanged.
func TestStreamReleasesTextThatOnlyLookedLikeAPlaceholder(t *testing.T) {
	r := NewRedactor(&Detector{})
	rd := redact(t, r, body(t, "sys", user(textBlock(positives()[3].text))))

	for _, pieces := range [][]string{
		{"The project is called JANUS", " and it is a proxy."},
		{"JANUS_", "CONFIG is the variable."},
		{"ends with JANUS_RE"}, // released when the block stops
		{"J", "A", "N", "U", "S", " spelled out"},
	} {
		out, restorer := restoreStream(t, rd, sse("text_delta", "text", pieces...), false)
		got, sawError := joined(t, out)
		if got != strings.Join(pieces, "") || sawError || restorer.Err != nil {
			t.Errorf("%q: got %q, error event %v", pieces, got, sawError)
		}
	}
}

func TestStreamStopsOnWhatItCannotRestore(t *testing.T) {
	r := NewRedactor(&Detector{})
	key := positives()[3]
	rd := redact(t, r, body(t, "sys", user(textBlock(key.text))))
	good := r.placeholder(key.want)

	cases := map[string]string{
		"a placeholder janus never issued": "run with " + placeholderPrefix + "0123456789ab now",
		"a placeholder cut off by the end": "run with " + good[:len(good)-3],
		"a damaged placeholder":            "run with " + placeholderPrefix + "zzzz now",
	}
	for name, text := range cases {
		// In a tool call the token would become a command or an edit, so
		// the turn is stopped.
		args := `{"command":"` + text + `"}`
		cut := len(args) - 6
		out, restorer := restoreStream(t, rd, sse("input_json_delta", "partial_json", args[:cut], args[cut:]), false)
		_, sawError := joined(t, out)
		if !sawError || !errors.Is(restorer.Err, ErrUnresolvable) {
			t.Errorf("%s: error event %v, Err %v", name, sawError, restorer.Err)
		}
		// The stream ends at the error: no message_stop follows to tell
		// the client the turn completed.
		if strings.Contains(out, "message_stop") || !strings.HasSuffix(out, "\n\n") || !strings.Contains(out, "janus: ") {
			t.Errorf("%s: after the error the stream went on:\n%s", name, out)
		}
		if strings.Contains(out, key.want) {
			t.Errorf("%s: a secret was sent in a turn that was stopped", name)
		}

		// In prose nothing acts on it. A session about janus itself writes
		// such things all the time, and must not be cut off for it.
		for deltaType, field := range map[string]string{"text_delta": "text", "thinking_delta": "thinking"} {
			out, restorer := restoreStream(t, rd, sse(deltaType, field, text[:12], text[12:]), false)
			got, sawError := joined(t, out)
			if got != text || sawError || restorer.Err != nil || !strings.Contains(out, "message_stop") {
				t.Errorf("%s in %s: got %q, error event %v, Err %v", name, deltaType, got, sawError, restorer.Err)
			}
		}
	}
}

// The prefix on its own, in something the client sent, is the client's
// text: a tool call that repeats it is searching for it, not using a
// secret.
func TestPartialPlaceholderFromTheClientIsNotAnError(t *testing.T) {
	r := NewRedactor(&Detector{})
	key := positives()[3]
	rd := redact(t, r, body(t, "sys", user(textBlock(key.text+" and grep for "+placeholderPrefix+" in the docs"))))

	args := `{"command":"grep -rn ` + placeholderPrefix + ` docs"}`
	out, restorer := restoreStream(t, rd, sse("input_json_delta", "partial_json", args[:20], args[20:]), false)
	if got, sawError := joined(t, out); got != args || sawError || restorer.Err != nil {
		t.Errorf("got %q, error event %v, Err %v", got, sawError, restorer.Err)
	}
}

func TestHeldBack(t *testing.T) {
	whole := placeholderPrefix + "0123456789ab"
	for s, want := range map[string]int{
		"":                              0,
		"hello":                         0,
		"hello J":                       1,
		"hello JANUS":                   5,
		"x " + placeholderPrefix:        len(placeholderPrefix),
		"x " + whole[:len(whole)-1]:     len(whole) - 1,
		"x " + whole:                    0, // whole: nothing to wait for
		"x " + placeholderPrefix + "0g": 0,
		"JANUS_REDACTED_0123456789abJ":  1,
	} {
		if got := heldBack(s); got != want {
			t.Errorf("heldBack(%q) = %d, want %d", s, got, want)
		}
	}
}
