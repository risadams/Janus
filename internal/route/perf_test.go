package route

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/risadams/janus/internal/backend"
	"github.com/risadams/janus/internal/guard"
	"github.com/risadams/janus/internal/guard/guardtest"
	"github.com/risadams/janus/internal/proxy"
)

// conversationOf builds a request of about the given size, shaped like one
// from Claude Code's main loop: a long system prompt, forty tools, and a
// history of tool calls and their results. It holds a few secrets and one
// read of a protected file, so that the guard has work to do.
func conversationOf(bytes int) []byte {
	filler := strings.Repeat("func handle(w http.ResponseWriter, r *http.Request) { log.Printf(\"%s %s\", r.Method, r.URL.Path) }\n", 40)
	secrets := guardtest.Secrets()
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

	var tools []map[string]any
	for i := 0; i < 40; i++ {
		tools = append(tools, map[string]any{"name": fmt.Sprintf("tool_%d", i), "description": strings.Repeat("Does a thing. ", 30),
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}})
	}
	messages := []map[string]any{{"role": "user", "content": "have a look around and fix the build"}}
	turn := 0
	add := func(path, result string) {
		id := fmt.Sprintf("toolu_%04d", turn)
		turn++
		messages = append(messages,
			map[string]any{"role": "assistant", "content": []map[string]any{
				text("Reading the next file."),
				{"type": "tool_use", "id": id, "name": "tool_1", "input": map[string]any{"path": path}}}},
			map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": result}}})
	}
	add("/app/deploy/settings.py", secrets[3].Text+"\n"+secrets[14].Text)
	add("/app/secrets/plan.md", "the merger closes on the ninth")
	base := map[string]any{"model": "claude-opus-5-5", "stream": true, "max_tokens": 32000,
		"system": []map[string]any{text(strings.Repeat("You are a careful engineer. ", 400))}, "tools": tools}
	size := func() int {
		base["messages"] = messages
		b, _ := json.Marshal(base)
		return len(b)
	}
	per := len(filler) + 500
	for n := (bytes - size()) / per; n > 0; n-- {
		// Every file is different, as real ones are.
		add(fmt.Sprintf("/app/src/file_%d.go", turn), strings.ReplaceAll(filler, "handle", fmt.Sprintf("handle%d", turn)))
	}
	base["messages"] = messages
	out, _ := json.Marshal(base)
	return out
}

type timing struct{ p50, p95 time.Duration }

func percentiles(samples []time.Duration) timing {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return timing{samples[len(samples)/2], samples[len(samples)*95/100]}
}

// TestMeasureOverhead prints how long a request takes through janus with
// each feature on, against the same request sent straight to the same
// server. It is a measurement, not a check, and runs only when asked:
//
//	JANUS_PERF=1 go test ./internal/route -run TestMeasureOverhead -v
//
// The results are written up in docs/research/performance.md.
func TestMeasureOverhead(t *testing.T) {
	if os.Getenv("JANUS_PERF") == "" {
		t.Skip("set JANUS_PERF=1 to run the measurement")
	}
	const runs = 300

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.ReplaceAll(localStream, "qwen/qwen3.8-27b", "claude-opus-5-5"))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	policy, _ := guard.NewPolicy([]string{"secrets/**", ".env*", "*.pem"}, []string{"COMPANY CONFIDENTIAL"})
	rules := []Rule{
		{Name: "small subagents", Class: Subagent, MaxTokens: 100, Route: RouteLocal, Local: qwen["janus-local"]},
		{Name: "rest", Route: RouteUpstream},
	}

	configs := []struct {
		name string
		opts func(o *proxy.Options, r *Router)
	}{
		{"pass-through, nothing on", func(o *proxy.Options, r *Router) {}},
		{"routing rules", func(o *proxy.Options, r *Router) { r.Rules = rules; o.Plan = r.Plan }},
		{"captures kept", func(o *proxy.Options, r *Router) { o.KeepBodies = true }},
		{"redaction", func(o *proxy.Options, r *Router) { o.Redactor = guard.NewRedactor(&guard.Detector{}) }},
		{"redaction with entropy search", func(o *proxy.Options, r *Router) {
			o.Redactor = guard.NewRedactor(&guard.Detector{Entropy: true})
		}},
		{"local-only policy", func(o *proxy.Options, r *Router) { o.Policy = policy }},
		{"everything", func(o *proxy.Options, r *Router) {
			r.Rules = rules
			o.Plan, o.KeepBodies, o.Policy = r.Plan, true, policy
			o.Redactor = guard.NewRedactor(&guard.Detector{Entropy: true})
		}},
	}

	client := &http.Client{}
	turn := 0
	send := func(target string, body []byte) time.Duration {
		// Each request is the conversation so far and one new turn of
		// about 4 KB that janus has not seen, as in a real session.
		turn++
		next := fmt.Sprintf(`,{"role":"user","content":"turn %d: %s"}]}`, turn, strings.Repeat("and then look at the next thing. ", 120))
		grown := string(body[:len(body)-2]) + next
		req, _ := http.NewRequest("POST", target+"/v1/messages", strings.NewReader(grown))
		req.Header.Set("Authorization", "Bearer token")
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		return time.Since(start)
	}
	// measure returns how long the first request took, when janus has
	// seen none of the conversation, and the spread of those after it.
	measure := func(target string, body []byte, runs int) (first time.Duration, later timing) {
		first = send(target, body)
		for i := 0; i < 10; i++ {
			send(target, body) // settle the connection
		}
		samples := make([]time.Duration, 0, runs)
		for i := 0; i < runs; i++ {
			samples = append(samples, send(target, body))
		}
		return first, percentiles(samples)
	}

	fmt.Printf("\n%s/%s, %d CPUs, %s, %d runs each\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(), runs)
	for _, size := range []int{30 << 10, 300 << 10, 3 << 20} {
		body := conversationOf(size)
		runs := runs
		if size > 1<<20 {
			runs = 60
		}
		_, direct := measure(up.URL, body, runs)
		fmt.Printf("\nRequest of %d KB. Direct: p50 %.2f ms, p95 %.2f ms\n", len(body)>>10, ms(direct.p50), ms(direct.p95))
		fmt.Printf("| %-30s | %12s | %12s | %12s |\n|---|---|---|---|\n", "Through janus with", "first request", "added at p50", "added at p95")
		for _, c := range configs {
			local, _ := lmStudio(t, nil)
			r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local, Aliases: qwen}
			opts := proxy.Options{Upstream: r.Upstream, Done: func(proxy.Exchange) {}}
			c.opts(&opts, r)
			janus := httptest.NewServer(proxy.New(opts))
			first, got := measure(janus.URL, body, runs)
			janus.Close()
			fmt.Printf("| %-30s | %9.2f ms | %9.2f ms | %9.2f ms |\n", c.name, ms(first), ms(got.p50-direct.p50), ms(got.p95-direct.p95))
		}
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// TestSoak sends a long run of requests through janus with everything on,
// several at a time, and reports whether memory and goroutines settle. It
// stands in for hours of use, and like TestMeasureOverhead runs only when
// asked:
//
//	JANUS_PERF=1 go test ./internal/route -run TestSoak -v -timeout 30m
func TestSoak(t *testing.T) {
	if os.Getenv("JANUS_PERF") == "" {
		t.Skip("set JANUS_PERF=1 to run the soak")
	}
	const total, workers = 40000, 8

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.ReplaceAll(localStream, "qwen/qwen3.8-27b", "claude-opus-5-5"))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	local, got := lmStudio(t, nil)
	go func() {
		for range got {
		}
	}()
	policy, _ := guard.NewPolicy([]string{"secrets/**"}, []string{"COMPANY CONFIDENTIAL"})
	r := &Router{Upstream: backend.NewUpstream(u, nil), Local: local, Aliases: qwen, Rules: []Rule{
		{Name: "every fifth", Model: "claude-haiku-5-5", Route: RouteEscalate, Local: qwen["janus-local"], TrialTimeout: time.Minute},
		{Name: "rest", Route: RouteUpstream},
	}}
	janus := httptest.NewServer(proxy.New(proxy.Options{
		Upstream: r.Upstream, Plan: r.Plan, KeepBodies: true, Policy: policy,
		Redactor: guard.NewRedactor(&guard.Detector{Entropy: true}),
		Done:     func(ex proxy.Exchange) { r.Report(ex) },
	}))
	defer janus.Close()

	body := conversationOf(100 << 10)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: workers}}
	snapshot := func() (heapMB float64, goroutines int) {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return float64(m.HeapAlloc) / (1 << 20), runtime.NumGoroutine()
	}

	jobs := make(chan int)
	failures := make(chan string, total)
	done := make(chan bool)
	for w := 0; w < workers; w++ {
		go func() {
			for i := range jobs {
				b := string(body[:len(body)-2]) + fmt.Sprintf(`,{"role":"user","content":"turn %d"}]}`, i)
				if i%5 == 0 { // some go to the local model first
					b = strings.Replace(b, `"model":"claude-opus-5-5"`, `"model":"claude-haiku-5-5"`, 1)
				}
				req, _ := http.NewRequest("POST", janus.URL+"/v1/messages", strings.NewReader(b))
				resp, err := client.Do(req)
				if err != nil {
					failures <- err.Error()
					continue
				}
				data, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || !strings.Contains(string(data), "message_stop") {
					failures <- fmt.Sprintf("status %d, %d bytes", resp.StatusCode, len(data))
				}
			}
			done <- true
		}()
	}

	start := time.Now()
	fmt.Printf("\n%d requests of %d KB, %d at a time, everything on\n| after | heap in use | goroutines |\n|---|---|---|\n", total, len(body)>>10, workers)
	for i := 0; i < total; i++ {
		if i%(total/8) == 0 && i > 0 {
			heap, routines := snapshot()
			fmt.Printf("| %6d | %8.1f MB | %4d |\n", i, heap, routines)
		}
		jobs <- i
	}
	close(jobs)
	for w := 0; w < workers; w++ {
		<-done
	}
	client.CloseIdleConnections()
	time.Sleep(200 * time.Millisecond)
	heap, routines := snapshot()
	fmt.Printf("| %6d | %8.1f MB | %4d |\n", total, heap, routines)
	fmt.Printf("%s in all, %.0f requests a second, %d failed\n", time.Since(start).Round(time.Second), float64(total)/time.Since(start).Seconds(), len(failures))
	if n := len(failures); n > 0 {
		t.Errorf("%d requests failed; the first: %s", n, <-failures)
	}
}
