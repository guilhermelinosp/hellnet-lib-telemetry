package telemetry

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/pprof"
	"net/url"
	"runtime"

	"github.com/grafana/pyroscope-go"
	"github.com/guilhermelinosp/hellnet-lib-environments/environments"
)

// pyroscopeProfiler é a minima superfície necessária do profiler Pyroscope para
// permitir parada no Shutdown sem acoplar telemetry.go à dependência.
type pyroscopeProfiler interface {
	Stop() error
}

// ProfileOption configura o profiler Pyroscope (push).
type ProfileOption func(*profileConfig)

type profileConfig struct {
	appName       string
	serverAddress string
}

// WithProfileAppName define o nome da aplicação reportada ao Pyroscope.
func WithProfileAppName(name string) ProfileOption {
	return func(c *profileConfig) { c.appName = name }
}

// WithProfileServer define o endpoint do Pyroscope/Alloy explicitamente,
// sobrescrevendo a derivação a partir de HELLNET_TELEMETRY_ENDPOINT.
//   - In-cluster: http://alloy:9999
//   - Via gateway: https://alloy.hellnet.com.br/ingest
func WithProfileServer(addr string) ProfileOption {
	return func(c *profileConfig) { c.serverAddress = addr }
}

// deriveProfileEndpoint deriva o endpoint do Pyroscope/Alloy a partir do
// HELLNET_TELEMETRY_ENDPOINT (OTLP), cobrindo as duas topologias do Alloy:
//   - In-cluster (com porta, ex.: http://alloy:4318) → mesma porta trocada para
//     9999 (pyroscope.receive_http): http://alloy:9999
//   - Gateway (sem porta, ex.: https://alloy.hellnet.com.br) → mesmo host com
//     path /ingest: https://alloy.hellnet.com.br/ingest
func deriveProfileEndpoint(base string) (string, error) {
	if base == "" {
		return "", errors.New("telemetry: HELLNET_TELEMETRY_ENDPOINT vazio; não é possível derivar o endpoint Pyroscope")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("telemetry: HELLNET_TELEMETRY_ENDPOINT inválido: %w", err)
	}
	if u.Port() != "" {
		// In-cluster: mesmo host, porta 9999 (pyroscope.receive_http do Alloy).
		u.Host = u.Hostname() + ":9999"
		u.Path = ""
	} else {
		// Gateway: same host, fixed /ingest path (Alloy's Pyroscope receiver
		// always listens on /ingest, regardless of the /v1/ OTLP path).
		u.Path = "/ingest"
	}
	return u.String(), nil
}

func defaultProfileConfig() profileConfig {
	endpoint := environments.GetString("HELLNET_TELEMETRY_", "HELLNET_", "ENDPOINT", "")
	addr, _ := deriveProfileEndpoint(endpoint)
	// Override explícito via HELLNET_TELEMETRY_PROFILE_ENDPOINT (opcional),
	// útil quando o pyroscope.receive_http do Alloy não usa a porta 9999.
	if custom := environments.GetString("HELLNET_TELEMETRY_", "HELLNET_", "PROFILE_ENDPOINT", ""); custom != "" {
		addr = custom
	}
	return profileConfig{
		appName:       environments.GetString("HELLNET_TELEMETRY_", "HELLNET_", "SERVICE", "telemetry"),
		serverAddress: addr,
	}
}

// ProfilesStart inicia o profiling contínuo PUSH para o Pyroscope/Alloy.
// O endpoint é derivado de HELLNET_TELEMETRY_ENDPOINT por padrão (mesmo host do
// OTLP); use WithProfileServer para override. O profiler para em Shutdown().
// Block e mutex profiling vêm SEMPRE habilitados (overhead desprezível em
// produção) — não é necessário passar options para isso.
//
// Exemplo (Alloy in-cluster, ENDPOINT=http://alloy:4318):
//
//	tel.ProfilesStart() // deriva http://alloy:9999
//
// Exemplo (via gateway, ENDPOINT=https://alloy.hellnet.com.br):
//
//	tel.ProfilesStart() // deriva https://alloy.hellnet.com.br/ingest
func (t *Telemetry) ProfilesStart(opts ...ProfileOption) (*pyroscope.Profiler, error) {
	if p, ok := t.profiler.(*pyroscope.Profiler); ok {
		return p, nil // já iniciado (idempotente)
	}
	cfg := defaultProfileConfig()
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.serverAddress == "" {
		return nil, errors.New("telemetry: não foi possível derivar o endpoint Pyroscope de HELLNET_TELEMETRY_ENDPOINT; use WithProfileServer")
	}
	// Block e mutex profiling sempre ligados (overhead desprezível em prod).
	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)
	prof, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: cfg.appName,
		ServerAddress:   cfg.serverAddress,
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileGoroutines,
			pyroscope.ProfileMutexCount,
			pyroscope.ProfileMutexDuration,
			pyroscope.ProfileBlockCount,
			pyroscope.ProfileBlockDuration,
		},
	})
	if err != nil {
		return nil, err
	}
	t.profiler = prof
	if t.Logger != nil {
		t.Logger.Info("telemetry: profiling Pyroscope iniciado", "profileEndpoint", cfg.serverAddress)
	}
	return prof, nil
}

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
