// Package capture writes the readable per-request logs: a Markdown render,
// the raw request and response, and the request as it was sent when Janus
// changed it. Captures hold prompts and code, so they are owner-only and
// pruned by age and size.
package capture
