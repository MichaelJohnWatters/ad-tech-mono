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
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime"
	"strconv"
)

// AttachPprof registers the standard pprof handlers on mux under /debug/pprof/.
// (pprof.Index also serves the named runtime profiles — /debug/pprof/block,
// /debug/pprof/mutex, heap, goroutine — no extra routes needed.)
func AttachPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// Arm/disarm the block+mutex profilers over HTTP — SetProfileRates had no
	// caller, so "armed on demand" was aspirational until now. Internal mux
	// only (like the rest of pprof). ?block=10000&mutex=5 to arm,
	// ?block=0&mutex=0 to disarm after capturing.
	mux.HandleFunc("/debug/pprof/rates", func(w http.ResponseWriter, r *http.Request) {
		block, _ := strconv.Atoi(r.URL.Query().Get("block"))
		mutex, _ := strconv.Atoi(r.URL.Query().Get("mutex"))
		SetProfileRates(block, mutex)
		fmt.Fprintf(w, "block_rate=%d mutex_fraction=%d\n", block, mutex)
	})
}

// SetProfileRates arms (or disarms, with 0/0) the block + mutex profilers —
// the two profile types that have runtime overhead while enabled. Flip on
// during an incident, capture via /debug/pprof/{block,mutex}, flip off.
func SetProfileRates(blockRate int, mutexFraction int) {
	runtime.SetBlockProfileRate(blockRate)
	runtime.SetMutexProfileFraction(mutexFraction)
}
