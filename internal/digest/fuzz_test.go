package digest

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// tool_output returns lines of an original numbered as they are in it, and
// only the lines asked for.
func FuzzExcerpt(f *testing.F) {
	f.Add("one\ntwo\nerror: three\n", 0, 0, "")
	f.Add("a\nb\nc\nd", 2, 3, "")
	f.Add("x\ny\nerror z\n", 0, 0, "^error")
	f.Add("", 5, 1, "(")
	f.Fuzz(func(t *testing.T, text string, from, to int, pattern string) {
		got, failed := excerpt(text, from, to, pattern)
		if failed || strings.HasPrefix(got, "no line between") {
			return
		}
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		lo, hi := max(from, 1), to
		if hi <= 0 || hi > len(lines) {
			hi = len(lines)
		}
		var re *regexp.Regexp
		if pattern != "" {
			re = regexp.MustCompile(pattern) // excerpt failed if it did not compile
		}
		for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			if strings.HasPrefix(line, "[cut short at line") {
				continue
			}
			var n int
			var rest string
			num, content, ok := strings.Cut(line, "\t")
			if _, err := fmt.Sscanf(strings.TrimSpace(num), "%d", &n); err != nil || !ok {
				t.Fatalf("line %q is not numbered", line)
			}
			rest = content
			if n < lo || n > hi {
				t.Fatalf("line %d is outside the range asked for, %d to %d", n, lo, hi)
			}
			if rest != lines[n-1] {
				t.Fatalf("line %d is %q in the output and %q in the original", n, rest, lines[n-1])
			}
			if re != nil && !re.MatchString(rest) {
				t.Fatalf("line %d does not match %q", n, pattern)
			}
		}
	})
}
