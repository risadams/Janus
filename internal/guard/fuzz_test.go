package guard

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

// However the text around a placeholder is cut into stream events, and
// however the stream is read, the secret comes back whole and nothing
// else changes.
func FuzzRestoreAnyCut(f *testing.F) {
	f.Add("Use ", " for the deploy.", uint8(3), uint8(9), uint8(2), false)
	f.Add("", "", uint8(0), uint8(0), uint8(0), true)
	f.Add("a\n\"quoted\"\\", " é ✓", uint8(200), uint8(1), uint8(17), true)
	f.Fuzz(func(t *testing.T, before, after string, cut1, cut2, readSize uint8, twice bool) {
		// Text that itself names a placeholder is another case: the model
		// repeating one Janus never issued is meant to stop the stream.
		if strings.Contains(strings.ToUpper(before+after), placeholderPrefix[:6]) {
			return
		}
		// A stream carries text as JSON strings, which cannot hold invalid
		// UTF-8; the test's own encoding would change such text before the
		// restorer saw it.
		if !utf8.ValidString(before) || !utf8.ValidString(after) {
			return
		}
		r := NewRedactor(&Detector{})
		key := positives()[3]
		rd, err := r.Redact(body(t, "sys", user(textBlock(key.text))))
		if err != nil {
			t.Fatal(err)
		}
		token := r.placeholder(key.want)
		full := before + token + after
		if twice {
			full += token
		}
		want := strings.ReplaceAll(full, token, key.want)

		a, b := int(cut1)%(len(full)+1), int(cut2)%(len(full)+1)
		if a > b {
			a, b = b, a
		}
		// Each piece of a stream is a JSON string of whole characters, so a
		// cut falls between characters, never inside one.
		for a > 0 && a < len(full) && !utf8.RuneStart(full[a]) {
			a--
		}
		for b > 0 && b < len(full) && !utf8.RuneStart(full[b]) {
			b--
		}
		var src io.Reader = strings.NewReader(sse("text_delta", "text", full[:a], full[a:b], full[b:]))
		if readSize%4 == 0 {
			src = iotest.OneByteReader(src)
		} else {
			src = iotest.HalfReader(src)
		}
		restorer := rd.Restore(io.NopCloser(src))
		out, err := io.ReadAll(restorer)
		if err != nil {
			t.Fatal(err)
		}
		got, sawError := joined(t, string(out))
		if got != want || sawError || restorer.Err != nil {
			t.Fatalf("cuts at %d and %d of %q: got %q, error event %v, err %v", a, b, full, got, sawError, restorer.Err)
		}
		if strings.Contains(string(out), placeholderPrefix) {
			t.Fatalf("a placeholder reached the client: %s", out)
		}
	})
}
