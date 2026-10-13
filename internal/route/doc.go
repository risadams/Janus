// Package route names each request's class and decides which backend
// answers it: the upstream, or a local model by alias or by rule, with the
// fallbacks and checks a rule asks for. Whatever goes upstream is repaired
// if a local model had a turn in its history, and rewritten by the
// transforms when those are on.
package route
