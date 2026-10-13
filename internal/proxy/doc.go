// Package proxy is the HTTP front door. It accepts the client's requests,
// asks the planner where each Messages request goes, applies the guard,
// and relays the chosen backend's response as it arrives, streaming
// included. A plan with a fallback is switched to it when its backend
// fails before anything reaches the client, and a local answer can be
// held back and checked before the client sees it.
//
// Every request ends in an Exchange: what was asked, what was decided and
// why, and what came back, for the ledger and the capture.
package proxy
