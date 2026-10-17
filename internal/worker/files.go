package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/risadams/janus/internal/anthropic"
	"github.com/risadams/janus/internal/mcp"
)

// Worker offers the local model's help with files under Root.
type Worker struct {
	Model *Model
	// Root is the directory the worker may read. Nothing outside it is
	// opened, whatever path it is given.
	Root string
	// Record is told about every call, for the ledger. May be nil.
	Record func(Call)
}

// Call is one use of a worker tool, as recorded.
type Call struct {
	Tool     string
	Start    time.Time
	Duration time.Duration
	// Files and BytesRead are what the local model was given to read.
	Files     int
	BytesRead int
	// ModelCalls is how many questions were put to the local model: one,
	// or one per chunk and one more to combine them.
	ModelCalls int
	Usage      anthropic.Usage
	// AnswerBytes is the size of what went back to Claude.
	AnswerBytes int
	// Failed says the call did not produce an answer; Why says what went
	// wrong.
	Failed bool
	Why    string
}

// Limits on one call. They keep a careless request from tying up the
// local model for an hour.
const (
	maxFiles     = 60
	maxReadBytes = 1 << 20 // about twenty-five minutes of reading at the measured rate
	maxChunks    = 30
	defaultWords = 300
	maxWords     = 1500
)

// Tools is the list to offer over MCP.
func (w *Worker) Tools() []mcp.Tool {
	return []mcp.Tool{{
		Name: "ask_files",
		Description: "Ask a question about one or more files and get back a short answer with file:line references. " +
			"A local model on this machine reads the files in full, so their contents never enter your context. " +
			"Use it for questions that can only be answered by reading everything: summarise these files, explain what this module does, " +
			"what do these documents say about a topic, how do these parts fit together. " +
			"Do NOT use it to find a string, an error, a name, or anything a search would find: search for that yourself, which takes seconds. " +
			"This tool is slow, about a minute for every 40 kB it reads, so prefer it only when the alternative is reading a lot of text you do not need verbatim. " +
			"Read a file yourself when you need its exact text, for example to edit it. " +
			"The answer is the local model's reading, not a quotation; check anything critical against the lines it cites.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"paths": {
					"type": "array",
					"items": {"type": "string"},
					"description": "Files to read, relative to the project root. Globs such as src/*.go or logs/*/error.log work; ** does not."
				},
				"question": {
					"type": "string",
					"description": "What you want to know. Be specific: say what to look for and what form the answer should take."
				},
				"max_words": {
					"type": "integer",
					"description": "Upper bound on the length of the answer. Default 300."
				}
			},
			"required": ["paths", "question"]
		}`),
		Call: w.askFiles,
	}}
}

func (w *Worker) askFiles(ctx context.Context, arguments json.RawMessage) (text string, failed bool) {
	call := Call{Tool: "ask_files", Start: time.Now()}
	defer func() {
		call.Duration = time.Since(call.Start)
		call.Failed, call.AnswerBytes = failed, len(text)
		if failed {
			call.Why = text
		}
		if w.Record != nil {
			w.Record(call)
		}
	}()

	var args struct {
		Paths    []string `json:"paths"`
		Question string   `json:"question"`
		MaxWords int      `json:"max_words"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return "The arguments were not understood: " + err.Error(), true
	}
	if strings.TrimSpace(args.Question) == "" || len(args.Paths) == 0 {
		return "Give both paths (a list of files or globs) and a question.", true
	}
	words := args.MaxWords
	switch {
	case words <= 0:
		words = defaultWords
	case words > maxWords:
		words = maxWords
	}

	files, notes, err := w.resolve(args.Paths)
	if err != nil {
		return err.Error(), true
	}
	if len(files) == 0 {
		return "None of those paths matched a readable text file. " + strings.Join(notes, " "), true
	}

	// How much the local model can take in one go: half its context, the
	// rest left for instructions and its answer, at three bytes a token.
	chunkBytes := w.Model.contextTokens(ctx) / 2 * 3
	chunkBytes = max(12<<10, min(chunkBytes, 300<<10))

	chunks, read, err := number(files, w.Root, chunkBytes)
	if err != nil {
		return err.Error(), true
	}
	call.Files, call.BytesRead = len(files), read
	if len(chunks) > maxChunks {
		return fmt.Sprintf("That is too much to read in one call: %d files, %s, which is %d passes for this model and the limit is %d. Narrow the paths.",
			len(files), size(read), len(chunks), maxChunks), true
	}

	ask := func(system, user string, tokens int) (string, error) {
		call.ModelCalls++
		answer, usage, err := w.Model.Ask(ctx, system, user, tokens)
		call.Usage.Input += usage.Input
		call.Usage.Output += usage.Output
		return answer, err
	}
	answerTokens := words*2 + 200

	var answer string
	if len(chunks) == 1 {
		answer, err = ask(answerSystem(words), "QUESTION: "+args.Question+"\n\nFILES:\n\n"+chunks[0], answerTokens)
	} else {
		// Too much for one pass: take notes on each part, then answer from
		// the notes.
		var gathered strings.Builder
		for i, chunk := range chunks {
			var part string
			part, err = ask(notesSystem, fmt.Sprintf("QUESTION: %s\n\nPART %d OF %d:\n\n%s", args.Question, i+1, len(chunks), chunk), 700)
			if err != nil {
				break
			}
			fmt.Fprintf(&gathered, "NOTES FROM PART %d:\n%s\n\n", i+1, part)
		}
		if err == nil {
			answer, err = ask(answerSystem(words), "QUESTION: "+args.Question+"\n\nNOTES TAKEN WHILE READING THE FILES:\n\n"+gathered.String(), answerTokens)
		}
	}
	if err != nil {
		return "The local model could not answer: " + err.Error() + ". Read the files yourself instead.", true
	}

	// Hold the answer to its bound whatever the model did.
	if limit := words * 9; len(answer) > limit {
		answer = answer[:limit] + "\n[cut off at the length limit]"
	}
	footer := fmt.Sprintf("\n\n[janus: %s read %d file%s, %s; this is its reading, not a quotation]", w.Model.ID, len(files), plural(len(files)), size(read))
	if len(notes) > 0 {
		footer += "\n[" + strings.Join(notes, " ") + "]"
	}
	return answer + footer, false
}

func answerSystem(words int) string {
	return fmt.Sprintf(`You read files for another AI agent that cannot see them, and answer its question.

- Answer the question directly. No preamble, no restating the question.
- Support each claim with where you found it, as path:line (for example src/main.go:42).
- If the files do not contain the answer, say so plainly. Do not guess and do not answer from general knowledge.
- Say what you are unsure of.
- At most %d words.`, words)
}

const notesSystem = `You are reading one part of a larger set of files, to help answer a question that will be answered later from your notes.

- Write down only what in this part bears on the question, each item with where it is, as path:line.
- If nothing in this part is relevant, write exactly: nothing relevant.
- Do not answer the question. Do not guess at what other parts contain.
- At most 250 words.`

// resolve turns the paths and globs given into files under the root. notes
// says what was left out and why.
func (w *Worker) resolve(patterns []string) (files []string, notes []string, err error) {
	root, err := filepath.EvalSymlinks(w.Root)
	if err != nil {
		return nil, nil, fmt.Errorf("the project root %s cannot be read: %v", w.Root, err)
	}
	seen := map[string]bool{}
	for _, pattern := range patterns {
		full := pattern
		if !filepath.IsAbs(full) {
			full = filepath.Join(root, pattern)
		}
		matches, globErr := filepath.Glob(full)
		if globErr != nil {
			return nil, nil, fmt.Errorf("%q is not a valid path or glob", pattern)
		}
		if len(matches) == 0 {
			notes = append(notes, fmt.Sprintf("%s matched nothing.", pattern))
		}
		sort.Strings(matches)
		for _, match := range matches {
			// Follow links before checking where the file really is, so
			// that a link inside the root cannot lead out of it.
			real, err := filepath.EvalSymlinks(match)
			if err != nil {
				continue
			}
			if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
				return nil, nil, fmt.Errorf("%s is outside the project root %s, and this tool only reads inside it", pattern, root)
			}
			info, err := os.Stat(real)
			switch {
			case err != nil:
				continue
			case info.IsDir():
				notes = append(notes, fmt.Sprintf("%s is a directory; name files or use a glob such as %s/*.", rel(root, real), rel(root, real)))
				continue
			case !info.Mode().IsRegular() || seen[real]:
				continue
			}
			seen[real] = true
			files = append(files, real)
		}
	}
	if len(files) > maxFiles {
		return nil, nil, fmt.Errorf("those paths match %d files and the limit is %d. Narrow them", len(files), maxFiles)
	}
	return files, notes, nil
}

// number reads the files and lays them out with line numbers, in chunks of
// about chunkBytes, each chunk saying which file its lines belong to.
// Files that are not text are left out.
func number(files []string, root string, chunkBytes int) (chunks []string, read int, err error) {
	root, _ = filepath.EvalSymlinks(root)
	var chunk strings.Builder
	flush := func() {
		if chunk.Len() > 0 {
			chunks = append(chunks, chunk.String())
			chunk.Reset()
		}
	}
	for _, path := range files {
		data, err := os.ReadFile(path) // #nosec G304 -- resolve has refused anything outside the project root, links included
		if err != nil {
			return nil, 0, fmt.Errorf("%s could not be read: %v", rel(root, path), err)
		}
		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			continue // binary
		}
		read += len(data)
		if read > maxReadBytes {
			return nil, 0, fmt.Errorf("those files come to more than %s, which is the most this tool reads in one call. Narrow the paths", size(maxReadBytes))
		}
		name := rel(root, path)
		header := fmt.Sprintf("=== %s ===\n", name)
		if chunk.Len() > 0 && chunk.Len()+len(header)+min(len(data), chunkBytes/4) > chunkBytes {
			flush()
		}
		chunk.WriteString(header)

		lines := bufio.NewScanner(bytes.NewReader(data))
		lines.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for n := 1; lines.Scan(); n++ {
			line := lines.Text()
			if len(line) > 2000 {
				line = line[:2000] + " [line cut]"
			}
			if chunk.Len()+len(line) > chunkBytes {
				flush()
				fmt.Fprintf(&chunk, "=== %s (continued) ===\n", name)
			}
			fmt.Fprintf(&chunk, "%d\t%s\n", n, line)
		}
		chunk.WriteString("\n")
	}
	flush()
	return chunks, read, nil
}

func rel(root, path string) string {
	// With forward slashes whatever the platform: these names are shown to
	// a model, and to Claude.
	if r, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(path)
}

func size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
