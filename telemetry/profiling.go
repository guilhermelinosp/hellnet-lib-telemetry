package telemetry

import (
	"net/http"
	"net/http/pprof"
)

// ProfilesRegister monta os handlers padrão de net/http/pprof no mux informado,
// sob /debug/pprof/, habilitando profiling PULL-based (CPU, heap, goroutine,
// block, mutex, trace). Aponte um scraper (ou `go tool pprof`) para
// /debug/pprof/ no mesmo mux que serve /metrics.
//
// Exemplo:
//
//	mux := http.NewServeMux()
//	tel.MetricsHandler()       // /metrics
//	tel.ProfilesRegister(mux)  // /debug/pprof/
func (t *Telemetry) ProfilesRegister(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}
