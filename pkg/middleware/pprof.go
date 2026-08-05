// pprof wiring — the on-demand profiler every service gets.
//
// Registration is free: the handlers cost nothing until a profile is actually
// pulled (CPU profiles sample only while captured; heap sampling is Go's
// always-on default). Block and mutex profiling DO cost when enabled, so they
// ship off and are armed per-incident via SetProfileRates.
//
// Exposure: these routes go on each service's INTERNAL mux (never Traefik).
// The 2026-08 throughput hunts repeatedly wanted block/goroutine profiles
// from hot services and found pprof only on cmd/pipeline — hence this helper.
package middleware

import (
	"net/http"
	"net/http/pprof"
	"runtime"
)

// AttachPprof registers the standard pprof handlers on mux under /debug/pprof/.
func AttachPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

// SetProfileRates arms (or disarms, with 0/0) the block + mutex profilers —
// the two profile types that have runtime overhead while enabled. Flip on
// during an incident, capture via /debug/pprof/{block,mutex}, flip off.
func SetProfileRates(blockRate int, mutexFraction int) {
	runtime.SetBlockProfileRate(blockRate)
	runtime.SetMutexProfileFraction(mutexFraction)
}
