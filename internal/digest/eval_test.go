package digest

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/config"
	"github.com/risadams/janus/internal/worker"
)

// TestEvaluateOnCaptures has the real local model digest every distinct
// tool result of DefaultDigestMinBytes or more in a capture directory, and
// writes one JSON line per result: its size, the digest's, the time taken,
// and how much of what matters survived. It is how docs/research/digest.md
// was measured.
//
//	JANUS_CAPTURES=~/logs/captures JANUS_DIGEST_EVAL=out.jsonl \
//	JANUS_LOCAL_URL=http://studio.local:1234 JANUS_DIGEST_MODEL=qwen/qwen3.8-27b \
//	go test ./internal/digest -run EvaluateOnCaptures -timeout 2h
func TestEvaluateOnCaptures(t *testing.T) {
	dir, out := os.Getenv("JANUS_CAPTURES"), os.Getenv("JANUS_DIGEST_EVAL")
	if dir == "" || out == "" {
		t.Skip("JANUS_CAPTURES and JANUS_DIGEST_EVAL are not set")
	}
	local, _ := url.Parse(os.Getenv("JANUS_LOCAL_URL"))
	model := &worker.Model{Local: backend.NewLocal(local, nil), ID: os.Getenv("JANUS_DIGEST_MODEL"),
		Timeout: config.DefaultDigestSeconds * 4 * time.Second} // measured, not cut off
	results := capturedResults(dir, config.DefaultDigestMinBytes)
	t.Logf("%d distinct tool results of %d bytes or more", len(results), config.DefaultDigestMinBytes)

	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i, r := range results {
		words := targetWords(len(r.text))
		start := time.Now()
		summary, usage, err := model.Ask(t.Context(), system(words), prompt(r.tool, r.text), words*3)
		line := map[string]any{
			"n": i, "tool": r.tool, "bytes": len(r.text), "lines": strings.Count(r.text, "\n") + 1,
			"seconds": time.Since(start).Seconds(), "input_tokens": usage.Input, "output_tokens": usage.Output,
		}
		if err != nil {
			line["error"] = err.Error()
		} else {
			kept, total := survives(r.text, summary)
			line["digest_bytes"], line["digest"] = len(summary), summary
			line["critical"], line["critical_kept"] = total, kept
		}
		data, _ := json.Marshal(line)
		f.Write(append(data, '\n'))
		t.Logf("%d/%d %s %d bytes -> %v in %.0fs", i+1, len(results), r.tool, len(r.text), line["digest_bytes"], line["seconds"])
	}
}

type captured struct{ tool, text string }

// capturedResults collects distinct tool results from captured requests.
func capturedResults(dir string, minBytes int) []captured {
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*v1-messages.request.txt"))
	sort.Strings(files)
	seen := map[string]bool{}
	var out []captured
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var req struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		names := map[string]string{}
		for _, m := range req.Messages {
			var blocks []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			}
			if json.Unmarshal(m.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "tool_use":
					names[b.ID] = b.Name
				case "tool_result":
					text, ok := plainText(b.Content)
					if !ok || b.IsError || len(text) < minBytes || seen[text] {
						continue
					}
					seen[text] = true
					out = append(out, captured{names[b.ToolUseID], text})
				}
			}
		}
	}
	return out
}

var (
	pathRe    = regexp.MustCompile(`[\w.-]*(?:/[\w.-]+)+\.\w{1,5}(?::\d+)?`)
	problemRe = regexp.MustCompile(`(?i)\b(error|fail(ed|ure)?|panic|fatal|warning|exception|denied|undefined|not found)\b`)
	wordRe    = regexp.MustCompile(`[A-Za-z_][\w.-]{3,}`)
)

// survives counts what matters in an output, and how much of it a digest
// kept: each distinct file path, and each line reporting a problem, which
// counts as kept when most of its words are in the digest. It is a rough
// measure, for comparing digests rather than proving one right.
func survives(original, digest string) (kept, total int) {
	lower := strings.ToLower(digest)
	paths := map[string]bool{}
	for _, p := range pathRe.FindAllString(original, -1) {
		paths[p] = true
	}
	for p := range paths {
		total++
		if strings.Contains(digest, p) || strings.Contains(digest, filepath.Base(p)) {
			kept++
		}
	}
	problems := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(original))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if problemRe.MatchString(line) && !problems[line] {
			problems[line] = true
		}
	}
	for line := range problems {
		words := wordRe.FindAllString(line, -1)
		if len(words) == 0 {
			continue
		}
		total++
		found := 0
		for _, w := range words {
			if strings.Contains(lower, strings.ToLower(w)) {
				found++
			}
		}
		if float64(found)/float64(len(words)) >= 0.6 {
			kept++
		}
	}
	return kept, total
}

func TestSurvives(t *testing.T) {
	original := "ok  pkg/a\nFAIL internal/x/y.go:12: undefined: Foo\nwrote cmd/main.go\n"
	kept, total := survives(original, "1 failure: internal/x/y.go:12 undefined Foo. Wrote main.go.")
	if total != 3 || kept != 3 {
		t.Errorf("kept %d of %d, want 3 of 3", kept, total)
	}
	if kept, _ := survives(original, "Some packages were built."); kept != 0 {
		t.Errorf("kept %d from a digest that names nothing, want 0", kept)
	}
}
