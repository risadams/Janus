// Package proxy is the HTTP front door: it accepts Messages API requests from the
// client, runs them through the pipeline, and relays the chosen backend's
// response, streaming included.
package proxy
