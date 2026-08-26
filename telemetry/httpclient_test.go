package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ─────────────── helpers do client HTTP instrumentado ───────────────

// newHTTPClientTel monta um Telemetry com tracer SDK real gravando em um
// spanRecorder E meter provider in-memory (ManualReader) — permite validar
// propagação de trace E métricas do client HTTP.
func newHTTPClientTel(t *testing.T) (*Telemetry, *spanRecorder, *sdkmetric.ManualReader) {
	t.Helper()

	rec := &spanRecorder{}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(rec)),
	)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	tel := &Telemetry{
		serviceName: "http-client-test",
		Tracer:      tp.Tracer("test"),
		tp:          tp,
		Meter:       meterAdapter{mp.Meter("test")},
		mp:          mp,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	return tel, rec, reader
}

// metricSumByLabels agrega todos os DataPoints de uma métrica SUM[int64],
// chaveando pelo valor de UM atributo informado (ex.: "status", "outcome").
func metricSumByLabels(t *testing.T, reader *sdkmetric.ManualReader, name, labelKey string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect(%s): %v", name, err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("métrica %s não é Sum[int64]", name)
			}
			for _, dp := range sum.DataPoints {
				key := ""
				if v, ok := dp.Attributes.Value(attribute.Key(labelKey)); ok {
					key = v.Emit()
				}
				out[key] += dp.Value
			}
		}
	}
	return out
}

// histogramCountByLabels soma o Count dos DataPoints de uma métrica HISTOGRAM,
// chaveado pelo valor de um atributo (ex.: outcome).
func histogramCountByLabels(t *testing.T, reader *sdkmetric.ManualReader, name, labelKey string) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect(%s): %v", name, err)
	}
	out := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("métrica %s não é Histogram", name)
			}
			for _, dp := range hist.DataPoints {
				key := ""
				if v, ok := dp.Attributes.Value(attribute.Key(labelKey)); ok {
					key = v.Emit()
				}
				out[key] += dp.Count
			}
		}
	}
	return out
}

func isTimeoutShaped(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// ──────────────────────────── testes ────────────────────────────

// TestHTTPClient_PropagatesTraceContext valida o coração da feature outbound:
// requests feitas pelo client carregam traceparent W3C válido que referencia
// EXATAMENTE o span CLIENT criado pelo transporte instrumentado, filho do
// span ativo no WithSpan do caller.
func TestHTTPClient_PropagatesTraceContext(t *testing.T) {
	tel, rec, _ := newHTTPClientTel(t)

	traceparentRe := regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-0[12]$`)
	gotHeader := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotHeader <- r.Header.Get("Traceparent"):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := tel.HTTPClient()

	err := tel.WithSpan("outbound-test", func(ctx context.Context) error {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if rerr != nil {
			return rerr
		}
		resp, derr := client.Do(req)
		if derr != nil {
			return derr
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	})
	if err != nil {
		t.Fatalf("WithSpan: %v", err)
	}

	select {
	case tp := <-gotHeader:
		m := traceparentRe.FindStringSubmatch(tp)
		if m == nil {
			t.Fatalf("traceparent inválido: %q (formato esperado 00-{trace}-{span}-{flags})", tp)
		}
		headerTraceID, headerSpanID := m[1], m[2]

		// O header deve referenciar um span REAL gravado pelo exporter in-memory.
		var clientSpan sdktrace.ReadOnlySpan
		for _, s := range rec.snapshot() {
			if s.SpanContext().SpanID().String() == headerSpanID &&
				s.SpanContext().TraceID().String() == headerTraceID {
				clientSpan = s
			}
		}
		if clientSpan == nil {
			t.Fatalf("nenhum span gravado corresponde ao traceparent %q (spans=%d)", tp, len(rec.snapshot()))
		}
		if clientSpan.SpanKind() != trace.SpanKindClient {
			t.Errorf("span do header tem kind=%v, want CLIENT", clientSpan.SpanKind())
		}
	default:
		t.Fatal("servidor não recebeu header Traceparent")
	}
}

// TestHTTPClient_PropagatesAsChildOfActiveSpan completa a validação de
// linhagem: o span CLIENT do outbound nasce FILHO do span do WithSpan caller
// (mesmo trace_id, parent.span_id correto).
func TestHTTPClient_PropagatesAsChildOfActiveSpan(t *testing.T) {
	tel, rec, _ := newHTTPClientTel(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := tel.HTTPClient()

	var outerSC trace.SpanContext
	err := tel.WithSpan("outbound-parent-check", func(ctx context.Context) error {
		outerSC = trace.SpanContextFromContext(ctx)
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if rerr != nil {
			return rerr
		}
		resp, derr := client.Do(req)
		if derr != nil {
			return derr
		}
		return func() error { defer func() { _ = resp.Body.Close() }(); return nil }()
	})
	if err != nil {
		t.Fatalf("WithSpan: %v", err)
	}
	if !outerSC.IsValid() {
		t.Fatal("ctx do WithSpan sem span válido")
	}

	var clientSpan sdktrace.ReadOnlySpan
	for _, s := range rec.snapshot() {
		if s.SpanKind() == trace.SpanKindClient && s.SpanContext().TraceID() == outerSC.TraceID() {
			clientSpan = s
		}
	}
	if clientSpan == nil {
		t.Fatal("nenhum span CLIENT no mesmo trace do span externo")
	}
	if got := clientSpan.Parent().SpanID(); got != outerSC.SpanID() {
		t.Errorf("clientSpan.Parent().SpanID() = %v, want %v (deve ser filho do span do caller)",
			got, outerSC.SpanID())
	}
}

// TestHTTPClient_RetriesIdempotentMethods: 503 duas vezes e depois 200 num GET
// ⇒ resposta final 200, exatamente 3 hits no servidor e métricas por tentativa
// corretas (counter por status; histograma com outcome retry/success).
func TestHTTPClient_RetriesIdempotentMethods(t *testing.T) {
	tel, _, reader := newHTTPClientTel(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := tel.HTTPClient(
		WithMaxRetries(2),
		WithRetryBackoff(time.Millisecond),
	)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get após retries: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("resposta = (%d,%q), want (200,\"ok\")", resp.StatusCode, string(body))
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("hits no servidor = %d, want 3 (503, 503, 200)", got)
	}

	statusCounts := metricSumByLabels(t, reader, "http_client_requests_total", "status")
	if statusCounts["503"] != 2 || statusCounts["200"] != 1 {
		t.Fatalf("requests_total por status = %v, want 503:2 e 200:1", statusCounts)
	}
	outcomeCounts := histogramCountByLabels(t, reader, "http_client_request_duration_seconds", "outcome")
	if outcomeCounts["retry"] != 2 || outcomeCounts["success"] != 1 {
		t.Fatalf("outcome por tentativa = %v, want retry:2 e success:1", outcomeCounts)
	}
}

// TestHTTPClient_DoesNotRetryPost garante a regra v1: métodos NÃO seguros
// (POST) nunca são repetidos — 1 hit apenas, mesmo diante de 503.
func TestHTTPClient_DoesNotRetryPost(t *testing.T) {
	tel, _, reader := newHTTPClientTel(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := tel.HTTPClient(
		WithMaxRetries(3),
		WithRetryBackoff(time.Millisecond),
	)

	resp, err := client.Post(srv.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 repassado ao caller", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits no servidor = %d, want 1 (POST não faz retry)", got)
	}

	statusCounts := metricSumByLabels(t, reader, "http_client_requests_total", "status")
	if statusCounts["503"] != 1 {
		t.Fatalf("requests_total por status = %v, want somente 1 tentativa (503:1)", statusCounts)
	}
	outcomeCounts := histogramCountByLabels(t, reader, "http_client_request_duration_seconds", "outcome")
	if outcomeCounts["error"] != 1 || outcomeCounts["retry"] != 0 {
		t.Fatalf("outcome por tentativa = %v, want error:1 sem retry", outcomeCounts)
	}
}

// TestHTTPClient_BaseTimeoutPerAttempt: com timeout por tentativa menor que a
// latência do servidor, o GET retriable esgota MaxRetries+1 tentativas e o
// erro final é timeout-shaped (o deadline por tentativa vaza como net.Timeout).
func TestHTTPClient_BaseTimeoutPerAttempt(t *testing.T) {
	tel, _, _ := newHTTPClientTel(t)

	const serverDelay = 500 * time.Millisecond
	arrivals := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrivals <- struct{}{} // registra a chegada ANTES de dormir (conta mesmo se o caller desistir)
		time.Sleep(serverDelay)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := tel.HTTPClient(
		WithBaseTimeout(50*time.Millisecond),
		WithMaxRetries(2),
		WithRetryBackoff(time.Millisecond),
	)

	start := time.Now()
	resp, err := client.Get(srv.URL)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("esperava erro de timeout, obtive sucesso")
	}
	if !isTimeoutShaped(err) {
		t.Fatalf("erro não é timeout-shaped: %v", err)
	}

	// Cada tentativa foi limitada a ~50ms ⇒ total bem menor que 3×500ms.
	if elapsed >= serverDelay {
		t.Errorf("elapsed = %v, want < %v (timeout por TENTATIVA, não acumulado)", elapsed, serverDelay)
	}

	// Esgotamento: MaxRetries+1 chegadas ao servidor.
	deadline := time.After(5 * time.Second)
	attempts := 0
	for attempts < 3 {
		select {
		case <-arrivals:
			attempts++
		case <-deadline:
			t.Fatalf("recebidas %d tentativas, want 3 (MaxRetries+1)", attempts)
		}
	}
	extra := time.NewTimer(100 * time.Millisecond)
	defer extra.Stop()
	select {
	case <-arrivals:
		t.Error("mais tentativas que MaxRetries+1")
	case <-extra.C:
	}
}

// TestHTTPClient_MetricsRegistered: chamada padrão registra ≥1 amostra em
// http_client_requests_total (contrato básico de observabilidade outbound).
func TestHTTPClient_MetricsRegistered(t *testing.T) {
	tel, _, reader := newHTTPClientTel(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := tel.HTTPClient()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = resp.Body.Close()

	total := int64(0)
	for _, v := range metricSumByLabels(t, reader, "http_client_requests_total", "status") {
		total += v
	}
	if total < 1 {
		t.Fatalf("http_client_requests_total = %d amostras somadas, want >= 1", total)
	}
}

// TestHTTPClient_DefaultOptionsSanity valida os defaults documentados
// (10s por tentativa; 2 retries = 3 tentativas; backoff 100ms dobrando até 5s
// com jitter ±20%) e o comportamento live de um GET simples.
func TestHTTPClient_DefaultOptionsSanity(t *testing.T) {
	// White-box: configuração resolvida com defaults.
	cfg := defaultHTTPClientConfig()
	if cfg.baseTimeout != 10*time.Second {
		t.Errorf("default baseTimeout = %v, want 10s", cfg.baseTimeout)
	}
	if cfg.maxRetries != 2 {
		t.Errorf("default maxRetries = %d, want 2", cfg.maxRetries)
	}
	if cfg.retryBackoff != 100*time.Millisecond {
		t.Errorf("default retryBackoff = %v, want 100ms", cfg.retryBackoff)
	}
	if cfg.extraTransport != nil {
		t.Errorf("default extraTransport = %v, want nil (clone de DefaultTransport)", cfg.extraTransport)
	}

	// Clamps das opções (valores inválidos são ignorados/normalizados).
	c2 := defaultHTTPClientConfig()
	WithMaxRetries(-1)(&c2)
	WithBaseTimeout(0)(&c2)
	WithRetryBackoff(-1)(&c2)
	if c2.maxRetries != 0 {
		t.Errorf("WithMaxRetries(-1) = %d, want 0", c2.maxRetries)
	}
	if c2.baseTimeout != 10*time.Second || c2.retryBackoff != 100*time.Millisecond {
		t.Errorf("opções zeradas/negativas devem preservar defaults; got %+v", c2)
	}

	// Sanidade de delay: cap em 5s mesmo com attempt alto; jitter ±20%.
	delay := retryDelay(100*time.Millisecond, 30)
	if delay > maxHTTPRetryBackoff {
		t.Errorf("retryDelay(attempt=30) = %v, want <= %v (cap)", delay, maxHTTPRetryBackoff)
	}
	base := time.Duration(float64(maxHTTPRetryBackoff) * 0.8)
	if delay < base {
		t.Errorf("retryDelay = %v, want >= %.0f%% do cap (jitter ±20%%)", delay, 80.0)
	}

	// Smoke live com defaults puros.
	tel, _, _ := newHTTPClientTel(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	resp, err := tel.HTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET com defaults: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status com defaults = %d, want 200", resp.StatusCode)
	}
}

// contandoRT é um RoundTripper decorador que conta passagens — usado para
// provar que WithExtraTransport liga o transporte customizado AO INNER da
// cadeia (abaixo do retry+otelhttp).
type countingRT struct {
	inner http.RoundTripper
	calls atomic.Int32
}

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	if c.inner == nil {
		return http.DefaultTransport.RoundTrip(req)
	}
	return c.inner.RoundTrip(req)
}

func TestHTTPClient_ExtraTransportWired(t *testing.T) {
	tel, _, reader := newHTTPClientTel(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	counting := &countingRT{}
	client := tel.HTTPClient(WithExtraTransport(counting), WithMaxRetries(0))

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = resp.Body.Close()
	if got := counting.calls.Load(); got != 1 {
		t.Fatalf("transporte customizado recebeu %d chamadas, want 1", got)
	}
	statusCounts := metricSumByLabels(t, reader, "http_client_requests_total", "status")
	if statusCounts["200"] != 1 {
		t.Fatalf("métricas ausentes com transporte customizado: %v", statusCounts)
	}
}
