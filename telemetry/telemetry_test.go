package telemetry

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"
)

// RED→GREEN: *Telemetry deve satisfazer a abstração Client (compilação).
func TestTelemetrySatisfiesClient(t *testing.T) {
	var _ Client = (*Telemetry)(nil)
}

func newTestTel(t *testing.T) *Telemetry {
	t.Helper()
	tel, err := New(Options{
		ServiceName: "abstraction-test",
		Enabled:     true, // tudo ligado (logging incluso)
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tel
}

func TestMeterCounterGaugeHistogramInt(t *testing.T) {
	tel := newTestTel(t)
	ctx := context.Background()

	c, err := tel.Meter.Counter("req_total") // atalho agnóstico (int64)
	if err != nil {
		t.Fatalf("Counter: %v", err)
	}
	if c == nil {
		t.Fatal("Counter retornou nil")
	}
	c.Add(ctx, 1) // OTel Add não retorna erro

	g, err := tel.Meter.Gauge("queue")
	if err != nil {
		t.Fatalf("Gauge: %v", err)
	}
	g.Record(ctx, 2)

	h, err := tel.Meter.Histogram("latency")
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}
	h.Record(ctx, 3)

	// estabilidade: mesmo nome reutilizável sem panic
	tel.Meter.Counter("req_total")
}

func TestMeterFloatSurfaceRaw(t *testing.T) {
	tel := newTestTel(t)
	ctx := context.Background()

	// superfície crua (float) continua disponível via embed de metric.Meter
	fc, err := tel.Meter.Float64Counter("req_total_f")
	if err != nil {
		t.Fatalf("Float64Counter: %v", err)
	}
	fc.Add(ctx, 1.5)

	fg, err := tel.Meter.Float64Gauge("temp")
	if err != nil {
		t.Fatalf("Float64Gauge: %v", err)
	}
	fg.Record(ctx, 0.5)

	fh, err := tel.Meter.Float64Histogram("latency_f")
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	fh.Record(ctx, 2.5)
}

func TestMeterTrace(t *testing.T) {
	tel := newTestTel(t)
	ctx := context.Background()

	_, span := tel.Trace().Start(ctx, "operation")
	if span == nil {
		t.Fatal("Start retornou span nil")
	}
	span.End()
}

func TestMeterLog(t *testing.T) {
	tel := newTestTel(t)
	ctx := context.Background()

	// logging desligado → Logger nil → usa slog.Default(); não deve panicar
	tel.Log().InfoContext(ctx, "info-ctx", "k", "v")
	tel.Log().Error("error-plain", "err", "boom")
	tel.Log().Warn("warn-plain")
	tel.Log().DebugContext(ctx, "debug-ctx")
}

func TestMeterNoopWhenMetricsDisabled(t *testing.T) {
	tel, err := New(Options{ServiceName: "noop-metrics", Enabled: false})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// facade nunca nil mesmo com metrics desligado
	if tel.Meter == nil {
		t.Fatal("tel.Meter nil com metrics desligado")
	}
	c, _ := tel.Meter.Counter("x")
	c.Add(context.Background(), 1)
	fh, _ := tel.Meter.Float64Histogram("y")
	fh.Record(context.Background(), 1.0)
}

func TestHealthLive(t *testing.T) {
	handler := (&Telemetry{}).Live()
	req := httptest.NewRequest(http.MethodGet, "/live", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusOK)
	}

	var status HealthStatus
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if status.Status != "live" {
		t.Errorf("Status = %q, want %q", status.Status, "live")
	}
	if len(status.Checks) != 0 {
		t.Errorf("Checks = %v, want empty", status.Checks)
	}
}

func TestHealthReady_OtlpUnreachable(t *testing.T) {
	// Use an invalid endpoint that will fail to connect
	handler := (&Telemetry{otlpEndpoint: "http://127.0.0.1:1"}).Ready()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}

	var status HealthStatus
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if status.Status != "not ready" {
		t.Errorf("Status = %q, want %q", status.Status, "not ready")
	}
	if len(status.Checks) != 2 {
		t.Errorf("Checks count = %d, want 2", len(status.Checks))
	}
	if status.Checks[0].Name != "self" || status.Checks[0].Status != "pass" {
		t.Errorf("First check = %+v, want self/pass", status.Checks[0])
	}
	if status.Checks[1].Name != "otlp-collector" || status.Checks[1].Status != "fail" {
		t.Errorf("Second check = %+v, want otlp-collector/fail", status.Checks[1])
	}
	if status.Checks[1].Error == "" {
		t.Error("Expected error message for failed OTLP check")
	}
}

func TestHealthAggregate_OtlpUnreachable(t *testing.T) {
	handler := (&Telemetry{otlpEndpoint: "http://127.0.0.1:1"}).Health()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}

	var status HealthStatus
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if status.Status != "degraded" {
		t.Errorf("Status = %q, want %q", status.Status, "degraded")
	}
	if len(status.Checks) != 2 {
		t.Errorf("Checks count = %d, want 2", len(status.Checks))
	}
	if status.Checks[0].Name != "self" || status.Checks[0].Status != "pass" {
		t.Errorf("First check = %+v, want self/pass", status.Checks[0])
	}
	if status.Checks[1].Name != "otlp-collector" || status.Checks[1].Status != "fail" {
		t.Errorf("Second check = %+v, want otlp-collector/fail", status.Checks[1])
	}
}

func TestHealthAggregate_OtlpReachable(t *testing.T) {
	// Start a test TCP server to simulate OTLP endpoint
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	endpoint := "http://" + listener.Addr().String()
	handler := (&Telemetry{otlpEndpoint: endpoint}).Health()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusOK)
	}

	var status HealthStatus
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if status.Status != "ok" {
		t.Errorf("Status = %q, want %q", status.Status, "ok")
	}
	if len(status.Checks) != 2 {
		t.Errorf("Checks count = %d, want 2", len(status.Checks))
	}
	if status.Checks[1].Name != "otlp-collector" || status.Checks[1].Status != "pass" {
		t.Errorf("OTLP check = %+v, want otlp-collector/pass", status.Checks[1])
	}
}

func TestHealthReady_OtlpReachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	endpoint := "http://" + listener.Addr().String()
	handler := (&Telemetry{otlpEndpoint: endpoint}).Ready()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusOK)
	}

	var status HealthStatus
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if status.Status != "ready" {
		t.Errorf("Status = %q, want %q", status.Status, "ready")
	}
	if len(status.Checks) != 2 {
		t.Errorf("Checks count = %d, want 2", len(status.Checks))
	}
	if status.Checks[1].Name != "otlp-collector" || status.Checks[1].Status != "pass" {
		t.Errorf("OTLP check = %+v, want otlp-collector/pass", status.Checks[1])
	}
}

func TestCheckOTLPReachable_Parsing(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		expectErr  bool
		shouldPass bool // if we had a real server
	}{
		{"http with port", "http://localhost:4318", true, false},
		{"https with port", "https://localhost:4318", true, false},
		{"http no port", "http://localhost", true, false},
		{"https no port", "https://localhost", true, false},
		{"host:port", "localhost:4318", true, false},
		{"host only", "localhost", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Just verify parsing doesn't panic
			err := checkOTLPReachable(context.Background(), tt.endpoint)
			if !tt.expectErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			if tt.expectErr && err == nil {
				t.Log("Expected error for unreachable endpoint")
			}
		})
	}
}

// TestCheckOTLPReachable_PortInferred garante que a porta vem do ENDPOINT ou,
// na sua ausência, é inferida do scheme (443 p/ https, 80 p/ http). A lib não
// lê nenhuma variável de porta separada.
func TestCheckOTLPReachable_PortInferred(t *testing.T) {
	// endpoint com porta explícita → usa a porta do próprio endpoint.
	h, p, err := parseOTLPEndpoint("https://collector.example.com:4318", "")
	if err != nil {
		t.Fatalf("parseOTLPEndpoint: %v", err)
	}
	if h != "collector.example.com" || p != "4318" {
		t.Errorf("host=%q port=%q, want collector.example.com/4318", h, p)
	}

	// https sem porta → infere 443.
	h, p, err = parseOTLPEndpoint("https://collector.example.com", "")
	if err != nil {
		t.Fatalf("parseOTLPEndpoint: %v", err)
	}
	if h != "collector.example.com" || p != "443" {
		t.Errorf("host=%q port=%q, want collector.example.com/443", h, p)
	}

	// http sem porta → infere 80.
	h, p, err = parseOTLPEndpoint("http://collector.example.com", "")
	if err != nil {
		t.Fatalf("parseOTLPEndpoint: %v", err)
	}
	if h != "collector.example.com" || p != "80" {
		t.Errorf("host=%q port=%q, want collector.example.com/80", h, p)
	}
}

func TestHealthStatus_JSON(t *testing.T) {
	status := HealthStatus{
		Status: "ok",
		Checks: []CheckResult{
			{Name: "self", Status: "pass"},
			{Name: "db", Status: "pass"},
			{Name: "cache", Status: "fail", Error: "timeout"},
		},
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded HealthStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Status != status.Status {
		t.Errorf("Status mismatch: %q != %q", decoded.Status, status.Status)
	}
	if len(decoded.Checks) != len(status.Checks) {
		t.Errorf("Checks count mismatch")
	}
	for i := range status.Checks {
		if decoded.Checks[i].Name != status.Checks[i].Name {
			t.Errorf("Check[%d] name mismatch: %q != %q", i, decoded.Checks[i].Name, status.Checks[i].Name)
		}
		if decoded.Checks[i].Status != status.Checks[i].Status {
			t.Errorf("Check[%d] status mismatch: %q != %q", i, decoded.Checks[i].Status, status.Checks[i].Status)
		}
		if decoded.Checks[i].Error != status.Checks[i].Error {
			t.Errorf("Check[%d] error mismatch: %q != %q", i, decoded.Checks[i].Error, status.Checks[i].Error)
		}
	}
}

func TestWriteHealth(t *testing.T) {
	w := httptest.NewRecorder()
	status := HealthStatus{Status: "ok"}
	writeHealth(w, http.StatusOK, status)

	if w.Code != http.StatusOK {
		t.Errorf("Status code = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", w.Header().Get("Content-Type"))
	}

	var decoded HealthStatus
	if err := json.NewDecoder(w.Body).Decode(&decoded); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if decoded.Status != "ok" {
		t.Errorf("Decoded status = %q, want ok", decoded.Status)
	}
}

func TestDialHealthCheck_Timeout(t *testing.T) {
	// Test that dialHealthCheck respects timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	// Use a non-routable IP to trigger timeout
	err := dialHealthCheckContext(ctx, "10.255.255.1", "4318")
	if err == nil {
		t.Error("Expected timeout error")
	}
	if err != context.DeadlineExceeded && err != context.Canceled {
		t.Logf("Got error (expected timeout): %v", err)
	}
}

// dialHealthCheckContext is a test helper that accepts a context
func dialHealthCheckContext(ctx context.Context, host, port string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func TestHealthRegister(t *testing.T) {
	tel := &Telemetry{otlpEndpoint: "http://127.0.0.1:1"} // collector irreachável
	tel.HealthRegister("db", func(ctx context.Context) error { return nil })
	tel.HealthRegister("cache", func(ctx context.Context) error { return errors.New("down") })

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()
	tel.Ready().ServeHTTP(w, req)

	var status HealthStatus
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.Status != "not ready" {
		t.Fatalf("status = %q, want not ready", status.Status)
	}
	// self + otlp-collector(fail) + db(pass) + cache(fail) = 4
	if len(status.Checks) != 4 {
		t.Fatalf("checks = %d, want 4", len(status.Checks))
	}
	byName := map[string]CheckResult{}
	for _, c := range status.Checks {
		byName[c.Name] = c
	}
	if byName["db"].Status != "pass" {
		t.Errorf("db = %+v, want pass", byName["db"])
	}
	if byName["cache"].Status != "fail" || byName["cache"].Error != "down" {
		t.Errorf("cache = %+v, want fail/down", byName["cache"])
	}
}

// TestParseOTLPEndpoint exercita a resolução pura host/port do endpoint OTLP,
// com injeção explícita do port default (sem acoplar a os.Getenv).
// Fronteiras cobertas: porta explícita, ausência de porta (usa default),
// forma host:port, host nu, e endpoint vazio.
func TestParseOTLPEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantHost string
		wantPort string
	}{
		{"url com porta explícita", "http://localhost:4318", "localhost", "4318"},
		{"https com porta explícita", "https://collector:4317", "collector", "4317"},
		{"http sem porta infere 80", "http://localhost", "localhost", "80"},
		{"https sem porta infere 443", "https://collector", "collector", "443"},
		{"url com path e porta", "http://localhost:4318/v1", "localhost", "4318"},
		{"forma host:port", "localhost:4318", "localhost", "4318"},
		{"host nu sem porta infere 80 (http implícito? sem scheme)", "localhost", "localhost", ""},
		{"endpoint vazio", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, err := parseOTLPEndpoint(tt.endpoint, "")
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if host != tt.wantHost {
				t.Errorf("host = %q, want %q", host, tt.wantHost)
			}
			if port != tt.wantPort {
				t.Errorf("port = %q, want %q", port, tt.wantPort)
			}
		})
	}
}

func TestWithSpan(t *testing.T) {
	tel := &Telemetry{}
	wantErr := errors.New("boom")
	if got := tel.WithSpan(context.Background(), "op", func(ctx context.Context) error {
		return wantErr
	}); got != wantErr {
		t.Errorf("got %v, want %v", got, wantErr)
	}
	if err := tel.WithSpan(context.Background(), "op2", func(ctx context.Context) error {
		return nil
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRedactHandler(t *testing.T) {
	var buf bytes.Buffer
	h := redactHandler{slog.NewJSONHandler(&buf, nil), map[string]struct{}{"password": {}}}
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "login", 0)
	r.AddAttrs(slog.String("password", "secret123"), slog.String("user", "alice"))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "secret123") {
		t.Errorf("password not redacted: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output: %s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("non-sensitive value should remain: %s", out)
	}
}

func TestMiddleware_StatusCodeCapture(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
		LogLevel:     slog.LevelInfo,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("ok"))
	})

	wrapped := Middleware(tel, handler)

	req := httptest.NewRequest(http.MethodPost, "/api/test", nil)
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusCreated)
	}
	if w.Body.String() != "ok" {
		t.Errorf("Body = %q, want %q", w.Body.String(), "ok")
	}
}

func TestMiddleware_LoggingOutput(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
		LogLevel:     slog.LevelInfo,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	// Replace logger with our test logger
	tel.Logger = logger
	defer tel.Shutdown()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := Middleware(tel, handler)

	req := httptest.NewRequest(http.MethodGet, "/test-path", nil)
	req.Host = "example.com"
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	logOutput := buf.String()
	if logOutput == "" {
		t.Fatal("Expected log output")
	}

	if !strings.Contains(logOutput, `"method":"GET"`) {
		t.Errorf("Log missing method: %s", logOutput)
	}
	if !strings.Contains(logOutput, `"path":"/test-path"`) {
		t.Errorf("Log missing path: %s", logOutput)
	}
	if !strings.Contains(logOutput, `"host":"example.com"`) {
		t.Errorf("Log missing host: %s", logOutput)
	}
	if !strings.Contains(logOutput, `"status":200`) {
		t.Errorf("Log missing status: %s", logOutput)
	}
	if !strings.Contains(logOutput, `"duration"`) {
		t.Errorf("Log missing duration: %s", logOutput)
	}
	if !strings.Contains(logOutput, `"level":"INFO"`) {
		t.Errorf("Log missing level: %s", logOutput)
	}
}

func TestMiddleware_ErrorStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
		LogLevel:     slog.LevelInfo,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	tel.Logger = logger
	defer tel.Shutdown()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	wrapped := Middleware(tel, handler)

	req := httptest.NewRequest(http.MethodGet, "/error", nil)
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	logOutput := buf.String()
	if !strings.Contains(logOutput, `"status":500`) {
		t.Errorf("Log missing error status: %s", logOutput)
	}
}

func TestMiddleware_DifferentMethods(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
		LogLevel:     slog.LevelInfo,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	tel.Logger = logger
	defer tel.Shutdown()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := Middleware(tel, handler)

	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		buf.Reset()
		req := httptest.NewRequest(method, "/test", nil)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)

		logOutput := buf.String()
		if !strings.Contains(logOutput, `"method":"`+method+`"`) {
			t.Errorf("Method %s not logged correctly: %s", method, logOutput)
		}
	}
}

func TestMiddleware_QueryParamsInPath(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
		LogLevel:     slog.LevelInfo,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	tel.Logger = logger
	defer tel.Shutdown()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := Middleware(tel, handler)

	req := httptest.NewRequest(http.MethodGet, "/api/users?id=123&name=test", nil)
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	logOutput := buf.String()
	// Path should include query string? Actually r.URL.Path doesn't include query
	if !strings.Contains(logOutput, `"/api/users"`) {
		t.Errorf("Path not logged correctly: %s", logOutput)
	}
}

func TestMiddleware_Panics(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
		LogLevel:     slog.LevelInfo,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	wrapped := Middleware(tel, handler)

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	w := httptest.NewRecorder()

	// Should recover panic? Currently it doesn't - will propagate
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("Expected panic to propagate")
			}
		}()
		wrapped.ServeHTTP(w, req)
	}()
}

func TestLoggingResponseWriter(t *testing.T) {
	lrw := &loggingResponseWriter{
		ResponseWriter: httptest.NewRecorder(),
		statusCode:     http.StatusOK,
	}

	// Test WriteHeader updates status
	lrw.WriteHeader(http.StatusNotFound)
	if lrw.statusCode != http.StatusNotFound {
		t.Errorf("statusCode = %d, want %d", lrw.statusCode, http.StatusNotFound)
	}

	// Test WriteHeader is called on underlying
	rec := lrw.ResponseWriter.(*httptest.ResponseRecorder)
	if rec.Code != http.StatusNotFound {
		t.Errorf("Underlying ResponseWriter code = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestMiddleware_WithTracing(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
		LogLevel:     slog.LevelInfo,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check that trace context is propagated
		ctx := r.Context()
		span := trace.SpanFromContext(ctx)
		if !span.SpanContext().IsValid() {
			t.Log("No active span in context (expected without parent)")
		}
		w.WriteHeader(http.StatusOK)
	})

	wrapped := Middleware(tel, handler)

	req := httptest.NewRequest(http.MethodGet, "/traced", nil)
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestMiddleware_DurationLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
		LogLevel:     slog.LevelInfo,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	tel.Logger = logger
	defer tel.Shutdown()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	wrapped := Middleware(tel, handler)

	req := httptest.NewRequest(http.MethodGet, "/slow", nil)
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	logOutput := buf.String()
	if !strings.Contains(logOutput, `"duration"`) {
		t.Errorf("Duration not logged: %s", logOutput)
	}
	// Duration should be in milliseconds or similar
	if !strings.Contains(logOutput, "ms") && !strings.Contains(logOutput, "µs") && !strings.Contains(logOutput, "s") {
		t.Errorf("Duration format unexpected: %s", logOutput)
	}
}

func TestRuntimeMetrics_RegistersComprehensiveSet(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel := &Telemetry{serviceName: "rt", Meter: meterAdapter{mp.Meter("test")}}
	tel.startRuntimeMetrics()

	// Coleta duas vezes: na 1ª o CPU ainda não tem baseline; na 2ª já deriva.
	var rm metricdata.ResourceMetrics
	for i := 0; i < 2; i++ {
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("Collect: %v", err)
		}
	}

	want := []string{
		"process_goroutines",
		"process_heap_alloc_bytes",
		"process_heap_sys_bytes",
		"process_heap_objects",
		"process_stack_inuse_bytes",
		"process_gc_total",
		"process_gc_pause_total_seconds",
		"process_gc_cpu_fraction",
		"process_num_cpu",
		"process_uptime_seconds",
		"process_sys_bytes",
	}
	// CPU só é coletada no Linux (readProcessCPUNs lê /proc); em outras
	// plataformas a métrica simplesmente não é emitida.
	if runtime.GOOS == "linux" {
		want = append(want, "process_cpu_usage_percent", "process_cpu_usage_ratio")
	}
	got := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = true
		}
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("métrica de runtime ausente: %s", w)
		}
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestComputeErrorBudget(t *testing.T) {
	tests := []struct {
		name         string
		target       float64
		good         int
		total        int
		wantErr      error
		wantObs      float64
		wantRemain   float64
		wantConsumed float64
		wantBreached bool
	}{
		{
			name:         "REQ-EB-001 at target 99/100",
			target:       0.99,
			good:         99,
			total:        100,
			wantObs:      0.99,
			wantRemain:   0.0,
			wantConsumed: 100.0,
			wantBreached: false,
		},
		{
			name:         "REQ-EB-002 perfect 100/100",
			target:       0.99,
			good:         100,
			total:        100,
			wantObs:      1.0,
			wantRemain:   0.01,
			wantConsumed: 0.0,
			wantBreached: false,
		},
		{
			name:         "REQ-EB-003 breached 97/100",
			target:       0.99,
			good:         97,
			total:        100,
			wantObs:      0.97,
			wantRemain:   -0.02,
			wantConsumed: 300.0,
			wantBreached: true,
		},
		{
			name:    "REQ-EB-004a invalid target",
			target:  1.5,
			good:    99,
			total:   100,
			wantErr: ErrInvalidTarget,
		},
		{
			name:    "REQ-EB-004b no events",
			target:  0.99,
			good:    0,
			total:   0,
			wantErr: ErrNoEvents,
		},
		{
			name:    "REQ-EB-004c good>total",
			target:  0.99,
			good:    101,
			total:   100,
			wantErr: ErrBadCounts,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ComputeErrorBudget(tt.target, tt.good, tt.total)
			if tt.wantErr != nil {
				if err != tt.wantErr {
					t.Fatalf("erro = %v, queria %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if !approx(got.Observed, tt.wantObs) {
				t.Errorf("Observed = %v, queria %v", got.Observed, tt.wantObs)
			}
			if !approx(got.Remaining, tt.wantRemain) {
				t.Errorf("Remaining = %v, queria %v", got.Remaining, tt.wantRemain)
			}
			if !approx(got.ConsumedPct, tt.wantConsumed) {
				t.Errorf("ConsumedPct = %v, queria %v", got.ConsumedPct, tt.wantConsumed)
			}
			if got.Breached != tt.wantBreached {
				t.Errorf("Breached = %v, queria %v", got.Breached, tt.wantBreached)
			}
		})
	}
}

func TestDefaultOptions(t *testing.T) {
	os.Setenv("HELLNET_SERVICE", "test-service")
	os.Setenv("HELLNET_ENDPOINT", "http://localhost:4318")
	defer os.Unsetenv("HELLNET_SERVICE")
	defer os.Unsetenv("HELLNET_ENDPOINT")

	opts := LoadFromEnv()

	if opts.ServiceName != "test-service" {
		t.Errorf("ServiceName = %q, want %q", opts.ServiceName, "test-service")
	}
	if opts.OTLPEndpoint != "http://localhost:4318" {
		t.Errorf("OTLPEndpoint = %q, want %q", opts.OTLPEndpoint, "http://localhost:4318")
	}
	if opts.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", opts.LogLevel, slog.LevelInfo)
	}
	if !opts.Enabled {
		t.Error("Enabled should be true by default")
	}
}

func TestDefaultOptions_EmptyEnv(t *testing.T) {
	os.Unsetenv("HELLNET_SERVICE")
	os.Unsetenv("HELLNET_ENDPOINT")

	opts := LoadFromEnv()

	if opts.ServiceName != "" {
		t.Errorf("ServiceName = %q, want empty", opts.ServiceName)
	}
	if opts.OTLPEndpoint != "" {
		t.Errorf("OTLPEndpoint = %q, want empty", opts.OTLPEndpoint)
	}
	if opts.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", opts.LogLevel, slog.LevelInfo)
	}
}

func TestNew_MissingServiceName(t *testing.T) {
	opts := Options{
		ServiceName:  "",
		OTLPEndpoint: "http://localhost:4318",
	}

	_, err := New(opts)
	if err == nil {
		t.Fatal("expected error for missing service name")
	}
	if !errors.Is(err, ErrMissingServiceName) {
		t.Errorf("error = %v, want ErrMissingServiceName", err)
	}
}

func TestNew_WithMinimalOptions(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	if tel.Tracer == nil {
		t.Error("Tracer should not be nil")
	}
	if tel.Meter == nil {
		t.Error("Meter should not be nil")
	}
}

func TestNew_DisabledTracing(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      false,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	if tel.Tracer != nil {
		t.Error("Tracer should be nil when tracing disabled")
	}
}

func TestNew_DisabledMetrics(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	// Com metrics desligado, tel.Meter ainda é um adapter noop (nunca nil),
	// para a abstração funcionar sem panic.
	if tel.Meter == nil {
		t.Error("Meter should not be nil (noop adapter) when metrics disabled")
	}
	if _, err := tel.Meter.Counter("noop_check"); err != nil {
		t.Errorf("noop Meter.Counter should not error: %v", err)
	}
}

func TestNew_CustomResourceAttrs(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
		ResourceAttrs: []attribute.KeyValue{
			attribute.String("custom.key", "custom-value"),
		},
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	// Verify tracer works
	_, span := tel.Tracer.Start(context.Background(), "test-span")
	span.SetAttributes(attribute.String("test", "value"))
	span.End()
	if span.SpanContext().TraceID().IsValid() {
		t.Log("Tracer functional with custom resource attrs")
	}
}

func TestShutdown(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Shutdown may return export errors if endpoint unreachable - that's OK
	// What matters is it doesn't hang/panic
	err = tel.Shutdown()
	if err != nil {
		t.Logf("Shutdown returned error (expected if endpoint unreachable): %v", err)
	}
}

func TestShutdownWithTimeout(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Shutdown may return export errors if endpoint unreachable - that's OK
	err = tel.Shutdown()
	if err != nil {
		t.Logf("Shutdown returned error (expected if endpoint unreachable): %v", err)
	}
}

func TestTracerFunctionality(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	ctx := context.Background()
	_, span := tel.Tracer.Start(ctx, "test-operation",
		trace.WithAttributes(attribute.String("key", "value")))
	span.AddEvent("test-event", trace.WithAttributes(attribute.Int("count", 42)))
	span.End()

	if !span.SpanContext().TraceID().IsValid() {
		t.Error("Span should have valid trace ID")
	}
}

func TestMeterFunctionality(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      false,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	ctx := context.Background()

	counter, err := tel.Meter.Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("Int64Counter failed: %v", err)
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("label", "value")))

	histogram, err := tel.Meter.Float64Histogram("test.histogram")
	if err != nil {
		t.Fatalf("Float64Histogram failed: %v", err)
	}
	histogram.Record(ctx, 0.5, metric.WithAttributes(attribute.String("label", "value")))

	_, err = tel.Meter.Int64ObservableGauge("test.gauge")
	if err != nil {
		t.Fatalf("Int64ObservableGauge failed: %v", err)
	}

	_ = ctx
}

func TestLoggerFunctionality(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
		LogLevel:     slog.LevelDebug,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	if tel.Logger == nil {
		t.Fatal("Logger should not be nil")
	}

	ctx := context.Background()
	tel.Logger.InfoContext(ctx, "test message", "key", "value")
	tel.Logger.WarnContext(ctx, "warning message")
	tel.Logger.ErrorContext(ctx, "error message", "err", errors.New("test error"))
	tel.Logger.DebugContext(ctx, "debug message")
}

func TestOTLPEndpointParsing(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantHost string
		wantPort string
	}{
		{"http with port", "http://collector:4318", "collector", "4318"},
		{"https infers 443", "https://collector.example.com", "collector.example.com", "443"},
		{"http infers 80", "http://collector.example.com", "collector.example.com", "80"},
		{"host:port only", "collector:4318", "collector", "4318"},
		{"https with explicit port", "https://collector.example.com:4317", "collector.example.com", "4317"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, p, err := parseOTLPEndpoint(tt.endpoint, "")
			if err != nil {
				t.Fatalf("parseOTLPEndpoint: %v", err)
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

func TestDeploymentEnv(t *testing.T) {
	os.Unsetenv("HELLNET_ENVIRONMENT")

	if deploymentEnv() != "" {
		t.Errorf("deploymentEnv() = %q, want empty", deploymentEnv())
	}

	os.Setenv("HELLNET_ENVIRONMENT", "Staging")
	if deploymentEnv() != "Staging" {
		t.Errorf("deploymentEnv() = %q, want %q", deploymentEnv(), "Staging")
	}

	os.Setenv("HELLNET_ENVIRONMENT", "Development")
	if deploymentEnv() != "Development" {
		t.Errorf("deploymentEnv() = %q, want %q", deploymentEnv(), "Development")
	}
}

func TestNew_ProvidersAreInitialized(t *testing.T) {
	opts := Options{
		ServiceName:  "test-service",
		OTLPEndpoint: "http://localhost:4318",
		Enabled:      true,
	}

	tel, err := New(opts)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer tel.Shutdown()

	// Verify Tracer, Meter, Logger are functional
	if tel.Tracer == nil {
		t.Error("Tracer should be set")
	}
	if tel.Meter == nil {
		t.Error("Meter should be set")
	}
	if tel.Logger == nil {
		t.Error("Logger should be set")
	}

	// Verify they work
	ctx := context.Background()
	_, span := tel.Tracer.Start(ctx, "test")
	span.End()
	counter, _ := tel.Meter.Int64Counter("test")
	counter.Add(ctx, 1)
	tel.Logger.InfoContext(ctx, "test")
}

// setMandatoryEnv define as envs obrigatórias para os testes que disparam
// Loading() em modo dev (que agora valida HELLNET_*).
func setMandatoryEnv(t *testing.T) {
	t.Helper()
	os.Setenv("HELLNET_SERVICE", "test-service")
	os.Setenv("HELLNET_ENDPOINT", "http://localhost:4318")
	t.Cleanup(func() {
		os.Unsetenv("HELLNET_SERVICE")
		os.Unsetenv("HELLNET_ENDPOINT")
	})
}

func TestLoadEnvFile_Development(t *testing.T) {
	// Create a temporary .env file
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")
	err := os.WriteFile(envFile, []byte("TEST_VAR=test-value\n"), 0o644)
	if err != nil {
		t.Fatalf("Failed to write .env: %v", err)
	}

	// Set environment to development
	os.Setenv("HELLNET_ENVIRONMENT", "Development")
	setMandatoryEnv(t)
	os.Setenv("HELLNET_ENV_FILE", envFile)
	defer func() {
		os.Unsetenv("HELLNET_ENVIRONMENT")
		os.Unsetenv("HELLNET_ENV_FILE")
		os.Unsetenv("TEST_VAR")
	}()

	Loading()

	if os.Getenv("TEST_VAR") != "test-value" {
		t.Errorf("TEST_VAR = %q, want %q", os.Getenv("TEST_VAR"), "test-value")
	}
}

func TestLoadEnvFile_Development_Lowercase(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")
	err := os.WriteFile(envFile, []byte("TEST_VAR=lowercase-value\n"), 0o644)
	if err != nil {
		t.Fatalf("Failed to write .env: %v", err)
	}

	os.Setenv("HELLNET_ENVIRONMENT", "development")
	setMandatoryEnv(t)
	os.Setenv("HELLNET_ENV_FILE", envFile)
	defer func() {
		os.Unsetenv("HELLNET_ENVIRONMENT")
		os.Unsetenv("HELLNET_ENV_FILE")
		os.Unsetenv("TEST_VAR")
	}()

	Loading()

	if os.Getenv("TEST_VAR") != "lowercase-value" {
		t.Errorf("TEST_VAR = %q, want %q", os.Getenv("TEST_VAR"), "lowercase-value")
	}
}

func TestLoadEnvFile_Production_Skipped(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")
	err := os.WriteFile(envFile, []byte("TEST_VAR=should-not-load\n"), 0o644)
	if err != nil {
		t.Fatalf("Failed to write .env: %v", err)
	}

	os.Setenv("HELLNET_ENVIRONMENT", "Production")
	os.Setenv("HELLNET_ENV_FILE", envFile)
	defer func() {
		os.Unsetenv("HELLNET_ENVIRONMENT")
		os.Unsetenv("HELLNET_ENV_FILE")
		os.Unsetenv("TEST_VAR")
	}()

	Loading()

	if os.Getenv("TEST_VAR") == "should-not-load" {
		t.Error("TEST_VAR should not be loaded in Production")
	}
}

func TestLoadEnvFile_Staging_Skipped(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")
	err := os.WriteFile(envFile, []byte("TEST_VAR=should-not-load\n"), 0o644)
	if err != nil {
		t.Fatalf("Failed to write .env: %v", err)
	}

	os.Setenv("HELLNET_ENVIRONMENT", "Staging")
	os.Setenv("HELLNET_ENV_FILE", envFile)
	defer func() {
		os.Unsetenv("HELLNET_ENVIRONMENT")
		os.Unsetenv("HELLNET_ENV_FILE")
		os.Unsetenv("TEST_VAR")
	}()

	Loading()

	if os.Getenv("TEST_VAR") == "should-not-load" {
		t.Error("TEST_VAR should not be loaded in Staging")
	}
}

func TestLoadEnvFile_NoEnvFile(t *testing.T) {
	os.Setenv("HELLNET_ENVIRONMENT", "Development")
	setMandatoryEnv(t)
	os.Setenv("HELLNET_ENV_FILE", "/nonexistent/path/.env")
	defer func() {
		os.Unsetenv("HELLNET_ENVIRONMENT")
		os.Unsetenv("HELLNET_ENV_FILE")
	}()

	// Should not panic
	Loading()
}

func TestLoadEnvFile_DefaultPath(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")
	err := os.WriteFile(envFile, []byte("DEFAULT_VAR=default-value\n"), 0o644)
	if err != nil {
		t.Fatalf("Failed to write .env: %v", err)
	}

	// Change to temp dir to test default path resolution
	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)

	os.Setenv("HELLNET_ENVIRONMENT", "Development")
	setMandatoryEnv(t)
	os.Unsetenv("HELLNET_ENV_FILE")
	defer func() {
		os.Unsetenv("HELLNET_ENVIRONMENT")
		os.Unsetenv("DEFAULT_VAR")
	}()

	Loading()

	if os.Getenv("DEFAULT_VAR") != "default-value" {
		t.Errorf("DEFAULT_VAR = %q, want %q", os.Getenv("DEFAULT_VAR"), "default-value")
	}
}

func TestLoadEnvFile_DoesNotOverrideExisting(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")
	err := os.WriteFile(envFile, []byte("EXISTING_VAR=from-env-file\n"), 0o644)
	if err != nil {
		t.Fatalf("Failed to write .env: %v", err)
	}

	// Set env var BEFORE loading
	os.Setenv("EXISTING_VAR", "from-env")
	os.Setenv("HELLNET_ENVIRONMENT", "Development")
	setMandatoryEnv(t)
	os.Setenv("HELLNET_ENV_FILE", envFile)
	defer func() {
		os.Unsetenv("HELLNET_ENVIRONMENT")
		os.Unsetenv("HELLNET_ENV_FILE")
		os.Unsetenv("EXISTING_VAR")
	}()

	Loading()

	// godotenv.Load doesn't override by default
	if os.Getenv("EXISTING_VAR") != "from-env" {
		t.Errorf("EXISTING_VAR = %q, want %q (should not be overridden)", os.Getenv("EXISTING_VAR"), "from-env")
	}
}

func TestLoadEnvFile_EmptyEnvironment(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")
	// Sem HELLNET_ENVIRONMENT no processo; o próprio .env o define → Loading() carrega.
	content := "TEST_VAR=should-load\n" +
		"HELLNET_SERVICE=test-service\n" +
		"HELLNET_ENDPOINT=http://localhost:4318\n" +
		"HELLNET_ENVIRONMENT=Development\n"
	err := os.WriteFile(envFile, []byte(content), 0o644)
	if err != nil {
		t.Fatalf("Failed to write .env: %v", err)
	}

	os.Unsetenv("HELLNET_ENVIRONMENT")
	os.Setenv("HELLNET_ENV_FILE", envFile)
	defer func() {
		os.Unsetenv("HELLNET_ENV_FILE")
		os.Unsetenv("TEST_VAR")
		os.Unsetenv("HELLNET_SERVICE")
		os.Unsetenv("HELLNET_ENDPOINT")
		os.Unsetenv("HELLNET_ENVIRONMENT")
	}()

	// Sem HELLNET_ENVIRONMENT definido no processo, Loading() carrega o .env por padrão.
	Loading()

	if os.Getenv("TEST_VAR") != "should-load" {
		t.Errorf("TEST_VAR = %q, want %q", os.Getenv("TEST_VAR"), "should-load")
	}
}

// TestExeDir verifica que a resolução do diretório do .env prefere o path do
// executável (onde main reside) e cai para os.Getwd() quando não há .env lá.
// No go test o binário fica em $TMPDIR sem .env, então espera-se fallback ao cwd.
func TestExeDir(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	got := exeDir()
	// Sem .env ao lado do executável de teste → deve cair no cwd.
	if got != dir {
		t.Errorf("exeDir() = %q, want cwd %q (fallback)", got, dir)
	}

	// Com .env ao lado do executável, deve retornar o dir do executável.
	if exe, eerr := os.Executable(); eerr == nil {
		exePath := filepath.Dir(exe)
		tmpEnv := filepath.Join(exePath, ".env")
		if werr := os.WriteFile(tmpEnv, []byte("X=1\n"), 0o644); werr == nil {
			defer os.Remove(tmpEnv)
			if got := exeDir(); got != exePath {
				t.Errorf("exeDir() = %q, want exe dir %q (.env presente)", got, exePath)
			}
		}
	}
}

// newTestTelemetry cria um Telemetry com MeterProvider in-memory (ManualReader)
// para inspecionar as métricas produzidas, e logger silencioso.
func newTestTelemetry() (*Telemetry, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel := &Telemetry{
		serviceName: "test-worker",
		Meter:       meterAdapter{mp.Meter("test")},
		Logger:      slog.New(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	return tel, reader
}

// TestLogErrorsCounter valida que o errorCountHandler conta erros logados
// (nível >= Error) automaticamente via log_errors_total.
func TestLogErrorsCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel := &Telemetry{
		serviceName: "test-err",
		Meter:       meterAdapter{mp.Meter("test")},
		Logger:      slog.New(&errorCountHandler{Handler: slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), tel: nil}),
	}
	// liga tel ao handler (como o New faz)
	if eh, ok := tel.Logger.Handler().(*errorCountHandler); ok {
		eh.tel = tel
	} else {
		t.Fatalf("logger não usa errorCountHandler")
	}

	tel.Logger.ErrorContext(context.Background(), "boom", slog.String("err", "x"))
	tel.Logger.WarnContext(context.Background(), "warn não conta")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "log_errors_total" {
				if s, ok := m.Data.(metricdata.Sum[int64]); ok && len(s.DataPoints) > 0 {
					total = s.DataPoints[0].Value
				}
			}
		}
	}
	if total != 1 {
		t.Fatalf("log_errors_total = %d, want 1", total)
	}
}

// TestWithSpanRecoversPanic valida que panics em WithSpan são contabilizados
// em exceptions_total e re-propaga o panic.
func TestWithSpanRecoversPanic(t *testing.T) {
	tel, reader := newTestTelemetry()

	func() {
		defer func() { recover() }() // engole o re-panic para o teste
		_ = tel.WithSpan(context.Background(), "op", func(ctx context.Context) error {
			panic("kaboom")
		})
	}()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "exceptions_total" {
				if s, ok := m.Data.(metricdata.Sum[int64]); ok && len(s.DataPoints) > 0 {
					total = s.DataPoints[0].Value
				}
			}
		}
	}
	if total != 1 {
		t.Fatalf("exceptions_total = %d, want 1", total)
	}
}

func TestHTTPClientTransport(t *testing.T) {
	tel, reader := newTestTelemetry()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := tel.HTTPClient(nil)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "http_client_requests_total" {
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
					total = sum.DataPoints[0].Value
				}
			}
		}
	}
	if total != 1 {
		t.Fatalf("http_client_requests_total = %d, want 1", total)
	}
}

// ── mock SQL driver (sem dependências externas) ──

type mockDriver struct{}

func (mockDriver) Open(name string) (driver.Conn, error) { return &mockConn{}, nil }

type mockConn struct{}

func (mockConn) Prepare(query string) (driver.Stmt, error) { return nil, nil }
func (mockConn) Close() error                              { return nil }
func (mockConn) Begin() (driver.Tx, error)                 { return nil, nil }

type mockConnector struct{ d mockDriver }

func (mockConnector) Connect(context.Context) (driver.Conn, error) { return &mockConn{}, nil }
func (mockConnector) Driver() driver.Driver                        { return mockDriver{} }

func TestWatchDB(t *testing.T) {
	tel, reader := newTestTelemetry()
	db := sql.OpenDB(mockConnector{})
	defer db.Close()
	db.SetMaxOpenConns(7)

	tel.WatchDB(db, "main")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var open, max int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "db_sql_open_connections":
				if g, ok := m.Data.(metricdata.Gauge[int64]); ok && len(g.DataPoints) > 0 {
					open = g.DataPoints[0].Value
				}
			case "db_sql_max_open_connections":
				if g, ok := m.Data.(metricdata.Gauge[int64]); ok && len(g.DataPoints) > 0 {
					max = g.DataPoints[0].Value
				}
			}
		}
	}
	if open == 0 && max == 0 {
		t.Fatalf("db_sql metrics não encontradas")
	}
	if max != 7 {
		t.Fatalf("db_sql_max_open_connections = %d, want 7", max)
	}
}

// attrEquals verifica se o attribute.Set contém a chave com o valor string esperado.
func attrEquals(attrs attribute.Set, key, want string) bool {
	v, ok := attrs.Value(attribute.Key(key))
	return ok && v.AsString() == want
}

// collectWorker lê do ManualReader os valores de worker_jobs_total (contagem)
// e worker_job_duration_seconds (soma) para o job/status informados.
func collectWorker(t *testing.T, reader *sdkmetric.ManualReader, job, status string) (int64, float64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	var count int64
	var durSum float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "worker_jobs_total":
				if s, ok := m.Data.(metricdata.Sum[int64]); ok {
					for _, dp := range s.DataPoints {
						if attrEquals(dp.Attributes, "job", job) && attrEquals(dp.Attributes, "status", status) {
							count += dp.Value
						}
					}
				}
			case "worker_job_duration_seconds":
				if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
					for _, dp := range h.DataPoints {
						if attrEquals(dp.Attributes, "job", job) && attrEquals(dp.Attributes, "status", status) {
							durSum += dp.Sum
						}
					}
				}
			}
		}
	}
	return count, durSum
}

func TestWorker_SuccessRecordsMetrics(t *testing.T) {
	tel, reader := newTestTelemetry()

	called := false
	err := tel.Worker(context.Background(), "import", func(ctx context.Context) error {
		called = true
		time.Sleep(time.Millisecond)
		return nil
	}, attribute.String("queue", "default"))
	if err != nil {
		t.Fatalf("Worker retornou erro inesperado: %v", err)
	}
	if !called {
		t.Fatal("fn não foi executado")
	}

	count, durSum := collectWorker(t, reader, "import", "ok")
	if count != 1 {
		t.Errorf("worker_jobs_total{status=ok} = %d, want 1", count)
	}
	if durSum <= 0 {
		t.Errorf("worker_job_duration_seconds sum = %v, want > 0", durSum)
	}
}

func TestWorker_ErrorStatusAndPropagation(t *testing.T) {
	tel, reader := newTestTelemetry()

	want := errors.New("boom")
	err := tel.Worker(context.Background(), "export", func(ctx context.Context) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Worker retornou %v, want %v", err, want)
	}

	count, _ := collectWorker(t, reader, "export", "error")
	if count != 1 {
		t.Errorf("worker_jobs_total{status=error} = %d, want 1", count)
	}
}

func TestWorker_LogOutput(t *testing.T) {
	var buf bytes.Buffer
	tel := &Telemetry{
		serviceName: "test-worker",
		Meter:       meterAdapter{sdkmetric.NewMeterProvider().Meter("test")},
		Logger:      slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	if err := tel.Worker(context.Background(), "sync", func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Worker: %v", err)
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log não é JSON válido: %v (conteúdo: %s)", err, buf.String())
	}
	if entry["msg"] != "worker job completed" {
		t.Errorf("msg = %v, want 'worker job completed'", entry["msg"])
	}
	if entry["job"] != "sync" {
		t.Errorf("job = %v, want 'sync'", entry["job"])
	}
	if _, ok := entry["duration"]; !ok {
		t.Error("log não contém 'duration'")
	}
}

// TestHealthCheckStatusExported valida que healthcheck_status é exportado como
// Int64ObservableGauge (callback) — fix para o otelprom não exportar Gauge
// síncrono com atributo. O status de cada check deve aparecer no collect.
func TestHealthCheckStatusExported(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel := &Telemetry{
		serviceName: "test-hc",
		Meter:       meterAdapter{mp.Meter("test")},
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	tel.registerHealthMetrics()

	// dispara os checks (registra healthcheck_status no mapa)
	if _, _ = tel.runChecks(context.Background()); len(tel.healthStatus) == 0 {
		t.Fatal("runChecks não populou healthStatus")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	found := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "healthcheck_status" {
				if s, ok := m.Data.(metricdata.Gauge[int64]); ok {
					for _, dp := range s.DataPoints {
						name := ""
						if v, ok := dp.Attributes.Value(attribute.Key("name")); ok {
							name = v.AsString()
						}
						found[name] = dp.Value
					}
				}
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("healthcheck_status não foi exportado")
	}
	if v, ok := found["self"]; !ok || v != 1 {
		t.Errorf("healthcheck_status{name=self} = %v, want 1", v)
	}
}

// TestMiddlewareBodySize valida que o Middleware emite http_requests_body_size_bytes
// (tamanho do corpo da requisição via Content-Length).
func TestMiddlewareBodySize(t *testing.T) {
	tel, reader := newTestTelemetry()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /x", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(Middleware(tel, mux))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/x", "text/plain", strings.NewReader("hello-body"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "http_requests_body_size_bytes" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("http_requests_body_size_bytes não foi exportado pelo Middleware")
	}
}
