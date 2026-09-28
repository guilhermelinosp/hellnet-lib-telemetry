package telemetry

import (
	"context"
	"go.uber.org/zap"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// newTestTel constrói um Telemetry sem collector real (endpoint vazio) para os
// testes unitários não tentarem conexões de rede.
func newTestTel(t *testing.T) *Telemetry {
	t.Helper()
	t.Setenv("HELLNET_TELEMETRY_SERVICE", "telemetry-test")
	t.Setenv("HELLNET_TELEMETRY_SERVICE_VERSION", "test")
	t.Setenv("HELLNET_TELEMETRY_ENDPOINT", "")
	t.Setenv("HELLNET_ENDPOINT", "")
	tel, err := New()
	if err != nil {
		t.Fatalf("New() retornou erro: %v", err)
	}
	t.Cleanup(func() { _ = tel.Close() })
	return tel
}

func TestNewAndClose(t *testing.T) {
	tel := newTestTel(t)
	if tel == nil {
		t.Fatal("New() retornou nil")
	}
	if tel.Meter == nil {
		t.Fatal("Meter não deve ser nil")
	}
	if tel.Logger == nil {
		t.Fatal("Logger não deve ser nil")
	}
	if err := tel.Close(); err != nil {
		t.Fatalf("Close() erro: %v", err)
	}
}

func TestMustNew(t *testing.T) {
	t.Setenv("HELLNET_TELEMETRY_SERVICE", "telemetry-test")
	t.Setenv("HELLNET_TELEMETRY_SERVICE_VERSION", "test")
	t.Setenv("HELLNET_TELEMETRY_ENDPOINT", "")
	tel := MustNew()
	if tel == nil {
		t.Fatal("MustNew() retornou nil")
	}
	_ = tel.Close()
}

// NewWithContext keeps configuration explicit and does not implicitly load a
// local .env file.
func TestNewWithContextUsesExplicitOptions(t *testing.T) {
	tel, err := NewWithContext(context.Background(), Options{
		ServiceName:  "test-svc",
		OTLPEndpoint: "http://test-collector:4318",
	})
	if err != nil {
		t.Fatalf("NewWithContext() erro: %v", err)
	}
	defer tel.Close()
	if tel.otlpEndpoint != "http://test-collector:4318" {
		t.Fatalf("endpoint = %q, want http://test-collector:4318", tel.otlpEndpoint)
	}
	if tel.serviceName != "test-svc" {
		t.Fatalf("serviceName = %q, want test-svc", tel.serviceName)
	}
}

func TestComputeErrorBudget(t *testing.T) {
	tests := []struct {
		name         string
		target       float64
		good         int
		total        int
		wantErr      error
		wantObs      float64
		wantRem      float64
		wantConsumed float64
		wantBreached bool
	}{
		{name: "100% success", target: 0.99, good: 100, total: 100, wantObs: 1, wantRem: 0.01, wantConsumed: 0, wantBreached: false},
		{name: "breached", target: 0.99, good: 95, total: 100, wantObs: 0.95, wantRem: -0.04, wantConsumed: 500, wantBreached: true},
		{name: "perfect target=1", target: 1.0, good: 100, total: 100, wantObs: 1, wantRem: 0, wantConsumed: 0, wantBreached: false},
		{name: "target=1 breached", target: 1.0, good: 99, total: 100, wantObs: 0.99, wantRem: 0, wantConsumed: 100, wantBreached: true},
		{name: "zero target", target: 0, good: 100, total: 100, wantErr: ErrInvalidTarget},
		{name: "above 1 target", target: 1.5, good: 100, total: 100, wantErr: ErrInvalidTarget},
		{name: "zero total", target: 0.99, good: 100, total: 0, wantErr: ErrNoEvents},
		{name: "negative good", target: 0.99, good: -1, total: 100, wantErr: ErrBadCounts},
		{name: "good > total", target: 0.99, good: 101, total: 100, wantErr: ErrBadCounts},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eb, err := ComputeErrorBudget(tt.target, tt.good, tt.total)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("esperado erro %v, got nil", tt.wantErr)
				}
				if err != tt.wantErr {
					t.Fatalf("erro = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if !approxEqual(eb.Observed, tt.wantObs) {
				t.Errorf("Observed = %v, want %v", eb.Observed, tt.wantObs)
			}
			if !approxEqual(eb.Remaining, tt.wantRem) {
				t.Errorf("Remaining = %v, want %v", eb.Remaining, tt.wantRem)
			}
			if !approxEqual(eb.ConsumedPct, tt.wantConsumed) {
				t.Errorf("ConsumedPct = %v, want %v", eb.ConsumedPct, tt.wantConsumed)
			}
			if eb.Breached != tt.wantBreached {
				t.Errorf("Breached = %v, want %v", eb.Breached, tt.wantBreached)
			}
		})
	}
}

// approxEqual compara floats com tolerância de 1e-9 (evita falhas por
// arredondamento de ponto flutuante em ConsumedPct/Remaining).
func approxEqual(a, b float64) bool {
	const eps = 1e-9
	return (a-b) < eps && (b-a) < eps
}

func TestOtlpSignalURL(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		signal string
		want   string
	}{
		{name: "empty", base: "", signal: "/v1/traces", want: ""},
		{name: "no path", base: "http://alloy:4318", signal: "/v1/traces", want: "http://alloy:4318/v1/traces"},
		{name: "root path", base: "http://alloy:4318/", signal: "/v1/traces", want: "http://alloy:4318/v1/traces"},
		// Integração com o gateway do Alloy: endpoint raiz + /v1/ PathPrefix.
		{name: "alloy gateway", base: "https://alloy.hellnet.com.br", signal: "/v1/traces", want: "https://alloy.hellnet.com.br/v1/traces"},
		{name: "invalid url", base: "://bad", signal: "/v1/traces", want: "://bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := otlpSignalURL(tt.base, tt.signal); got != tt.want {
				t.Errorf("otlpSignalURL(%q,%q) = %q, want %q", tt.base, tt.signal, got, tt.want)
			}
		})
	}
}

func TestParseOTLPEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantHost string
		wantPort string
		wantErr  bool
	}{
		{name: "host:port", endpoint: "http://alloy:4318", wantHost: "alloy", wantPort: "4318"},
		{name: "https default", endpoint: "https://alloy.hellnet.com.br", wantHost: "alloy.hellnet.com.br", wantPort: "443"},
		{name: "http default", endpoint: "http://alloy:4317", wantHost: "alloy", wantPort: "4317"},
		{name: "bare host", endpoint: "tempo:4317", wantHost: "tempo", wantPort: "4317"},
		{name: "host only", endpoint: "localhost", wantHost: "localhost", wantPort: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, p, err := parseOTLPEndpoint(tt.endpoint, "")
			if tt.wantErr {
				if err == nil {
					t.Fatal("esperado erro")
				}
				return
			}
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if h != tt.wantHost {
				t.Errorf("host = %q, want %q", h, tt.wantHost)
			}
			if p != tt.wantPort {
				t.Errorf("port = %q, want %q", p, tt.wantPort)
			}
		})
	}
}

func TestWithSpanAndLog(t *testing.T) {
	tel := newTestTel(t)
	var inner bool
	err := tel.WithSpan("op", func(ctx context.Context) error {
		inner = true
		logger := tel.Log(ctx)
		logger.Trace("trace", "k", "v")
		logger.Debug("debug", "k", "v")
		logger.Info("info", "k", "v")
		logger.Warn("warn", "k", "v")
		logger.Error("error", "k", "v")
		logger.Fatal("fatal", "k", "v")
		logger.Critical("critical", "k", "v")
		return nil
	})
	if err != nil {
		t.Fatalf("WithSpan erro: %v", err)
	}
	if !inner {
		t.Fatal("fn não foi chamada")
	}
}

func TestWithSpanContextPreservesParent(t *testing.T) {
	tel := newTestTel(t)
	parent := context.WithValue(context.Background(), struct{}{}, "parent")
	if err := tel.WithSpanContext(parent, "context-op", func(ctx context.Context) error {
		if got := ctx.Value(struct{}{}); got != "parent" {
			t.Fatalf("parent context value = %v, want parent", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithSpanContext() erro: %v", err)
	}
}

func TestSensitiveFieldsAreRedacted(t *testing.T) {
	fields := redactFields([]zap.Field{zap.String("authorization", "Bearer secret"), zap.String("order_id", "123")})
	if fields[0].String != "[REDACTED]" {
		t.Fatalf("authorization was not redacted: %q", fields[0].String)
	}
	if fields[1].String != "123" {
		t.Fatalf("non-sensitive field changed: %q", fields[1].String)
	}
}

func TestReadyDoesNotDependOnCollector(t *testing.T) {
	tel := newTestTel(t)
	tel.otlpEndpoint = "127.0.0.1:1"
	ready, allPass := tel.runChecks(context.Background(), false)
	if !allPass {
		t.Fatal("readiness should pass without checking the collector")
	}
	for _, check := range ready {
		if check.Name == "otlp-collector" {
			t.Fatal("readiness must not include the OTLP collector")
		}
	}
	health, _ := tel.runChecks(context.Background(), true)
	foundCollector := false
	for _, check := range health {
		if check.Name == "otlp-collector" {
			foundCollector = true
		}
	}
	if !foundCollector {
		t.Fatal("health should include the OTLP collector")
	}
}

func TestWithSpanPanic(t *testing.T) {
	tel := newTestTel(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("esperado panic re-propagado")
		}
	}()
	_ = tel.WithSpan("op", func(ctx context.Context) error {
		panic("boom")
	})
}

func TestWorker(t *testing.T) {
	tel := newTestTel(t)
	called := false
	err := tel.Worker("job", func(ctx context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("Worker erro: %v", err)
	}
	if !called {
		t.Fatal("worker fn não foi chamada")
	}
}

func TestWorkerError(t *testing.T) {
	tel := newTestTel(t)
	want := context.DeadlineExceeded
	err := tel.Worker("job", func(ctx context.Context) error {
		return want
	})
	if err != want {
		t.Fatalf("Worker erro = %v, want %v", err, want)
	}
}

func TestMiddleware(t *testing.T) {
	tel := newTestTel(t)
	h := Middleware(tel, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/foo")
	if err != nil {
		t.Fatalf("GET erro: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", resp.StatusCode)
	}
}

func TestHealthEndpoints(t *testing.T) {
	tel := newTestTel(t)
	for _, path := range []string{"/live", "/ready", "/health"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		tel.Live().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s: code = %d, want 200", path, w.Code)
		}
	}
}

func TestProfilesStartNoEndpoint(t *testing.T) {
	tel := newTestTel(t)
	// Sem endpoint configurado (newTestTel zera HELLNET_TELEMETRY_ENDPOINT),
	// ProfilesStart deve retornar erro (não conecta).
	prof, err := tel.ProfilesStart()
	if err == nil {
		t.Fatal("esperado erro com HELLNET_TELEMETRY_ENDPOINT vazio")
	}
	if prof != nil {
		t.Fatal("profiler não deve ser retornado em caso de erro")
	}
}

func TestDeriveProfileEndpoint(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{name: "in-cluster", base: "http://alloy:4318", want: "http://alloy:9999"},
		{name: "in-cluster root path", base: "http://alloy:4318/", want: "http://alloy:9999"},
		{name: "gateway", base: "https://alloy.hellnet.com.br", want: "https://alloy.hellnet.com.br/ingest"},
		{name: "gateway with v1", base: "https://alloy.hellnet.com.br/v1/", want: "https://alloy.hellnet.com.br/ingest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deriveProfileEndpoint(tt.base)
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if got != tt.want {
				t.Errorf("deriveProfileEndpoint(%q) = %q, want %q", tt.base, got, tt.want)
			}
		})
	}
	if _, err := deriveProfileEndpoint(""); err == nil {
		t.Fatal("esperado erro com base vazia")
	}
}

// TestProfilesStartIntegration valida o auto-start do push Pyroscope via New()
// (derivando o endpoint do HELLNET_TELEMETRY_ENDPOINT). Pulado a menos que
// ALLOY_ENDPOINT esteja definido (ex.: http://alloy:4318 ou https://alloy.hellnet.com.br).
func TestProfilesStartIntegration(t *testing.T) {
	endpoint := os.Getenv("ALLOY_ENDPOINT")
	if endpoint == "" {
		t.Skip("defina ALLOY_ENDPOINT para rodar a integração real com o Pyroscope/Alloy")
	}
	t.Setenv("HELLNET_TELEMETRY_ENDPOINT", endpoint)
	tel := MustNew() // auto-inicia ProfilesStart() internamente
	defer tel.Close()
	if tel.profiler == nil {
		t.Fatal("profiler não iniciou automaticamente no New()")
	}
	// dá tempo do profiler registrar/enviar o primeiro snapshot
	time.Sleep(2 * time.Second)
}

// TestAlloyIntegration valida o envio real de traces/metrics/logs para um
// collector Alloy. É pulado a menos que ALLOY_ENDPOINT esteja definido
// (ex.: http://alloy:4318 ou https://alloy.hellnet.com.br).
func TestAlloyIntegration(t *testing.T) {
	endpoint := os.Getenv("ALLOY_ENDPOINT")
	if endpoint == "" {
		t.Skip("defina ALLOY_ENDPOINT (ex.: http://alloy:4318) para rodar a integração real com o Alloy")
	}
	t.Setenv("HELLNET_TELEMETRY_ENDPOINT", endpoint)
	t.Setenv("HELLNET_TELEMETRY_SERVICE", "telemetry-test")
	tel := MustNew()
	defer tel.Close()

	tel.Log().Info("integration test log", "ok", true)
	if err := tel.WithSpan("integration-span", func(ctx context.Context) error {
		c, err := tel.Meter.Counter("integration_test_total")
		if err != nil {
			return err
		}
		c.Add(ctx, 1)
		return nil
	}); err != nil {
		t.Fatalf("WithSpan erro: %v", err)
	}

	// Tempo para os exporters batchearem e enviarem ao Alloy.
	time.Sleep(2 * time.Second)
}
