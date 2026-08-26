package telemetry

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
)

// ─────────────── HTTP client instrumentado (outbound) ───────────────
//
// Contraparte de saída do Middleware (que cobre o lado servidor via
// otelhttp.NewHandler): a factory HTTPClient devolve um *http.Client cujo
// transporte injeta propagação W3C (traceparent/baggage), cria um span CLIENT
// por tentativa (através de otelhttp.NewTransport), aplica retry com backoff
// que dobra a cada tentativa (teto 5s) + jitter sobre erros transitórios e
// emite métricas por tentativa (mesma família http_client_* do catálogo).
//
// Encadeamento do transporte (o retry é a camada MAIS EXTERNA para que cada
// tentativa passe pelo transporte instrumentado):
//
//	client.Transport = clientRetryTransport{next: otelhttp.NewTransport(inner)}
//
// O contexto usado nas tentativas deriva de req.Context() (linhagem que o
// caller tiver — ex.: filho do span atual dentro de WithSpan). NÃO há prazo
// total derivado do baseCtx deliberadamente: quem chama controla o ciclo via
// ctx próprio da request.

const (
	// defaultHTTPBaseTimeout limita CADA tentativa (inclui ler a resposta).
	defaultHTTPBaseTimeout = 10 * time.Second
	// defaultHTTPMaxRetries: 2 retries ⇒ até 3 tentativas por chamada.
	defaultHTTPMaxRetries = 2
	// defaultHTTPRetryBackoff é o delay inicial entre tentativas (dobra a cada
	// retry, com teto maxHTTPRetryBackoff e jitter ±retryJitterFraction).
	defaultHTTPRetryBackoff = 100 * time.Millisecond
	// maxHTTPRetryBackoff é o teto do delay entre tentativas.
	maxHTTPRetryBackoff = 5 * time.Second
	// retryJitterFraction é a amplitude do jitter ±20% aplicado ao delay
	// (evita sincronização de retries entre chamadas concorrentes).
	retryJitterFraction = 0.20
)

// clientMetrics agrega as primitivas OTel emitidas POR TENTATIVA. Os nomes
// seguem o mesmo catálogo do transporte antigo (http_client_*) para manter
// dashboards existentes funcionando.
type clientMetrics struct {
	requestsTotal    metric.Int64Counter       // http_client_requests_total{method,host,status}
	requestDuration  metric.Float64Histogram   // http_client_request_duration_seconds{method,host,outcome}
	requestsInflight metric.Int64UpDownCounter // http_client_requests_inflight{method,host}
}

// httpClientConfig concentra as opções resolvidas da factory (com defaults).
type httpClientConfig struct {
	baseTimeout    time.Duration     // prazo por tentativa
	maxRetries     int               // nº de RETRIES (tentativas = maxRetries+1)
	retryBackoff   time.Duration     // delay inicial (dobra com cap 5s + jitter)
	extraTransport http.RoundTripper // transporte interno customizado (opcional)
}

// defaultHTTPClientConfig devolve a configuração padrão da factory.
func defaultHTTPClientConfig() httpClientConfig {
	return httpClientConfig{
		baseTimeout:  defaultHTTPBaseTimeout,
		maxRetries:   defaultHTTPMaxRetries,
		retryBackoff: defaultHTTPRetryBackoff,
	}
}

// HTTPOption ajusta uma opção da factory HTTPClient.
type HTTPOption func(*httpClientConfig)

// WithBaseTimeout define o timeout POR TENTATIVA (default 10s). O prazo cobre
// a tentativa inteira — conexão, envio, resposta e leitura do body — portanto
// use valores maiores para downloads/streaming longos.
func WithBaseTimeout(d time.Duration) HTTPOption {
	return func(c *httpClientConfig) {
		if d > 0 {
			c.baseTimeout = d
		}
	}
}

// WithMaxRetries define quantos RETRIES após a primeira tentativa (default 2,
// ou seja 3 tentativas). 0 desliga o retry; negativo é tratado como 0.
func WithMaxRetries(n int) HTTPOption {
	return func(c *httpClientConfig) {
		if n < 0 {
			n = 0
		}
		c.maxRetries = n
	}
}

// WithRetryBackoff define o delay inicial entre tentativas (default 100ms),
// dobrando a cada retry até o teto de 5s, sempre com jitter ±20%.
func WithRetryBackoff(base time.Duration) HTTPOption {
	return func(c *httpClientConfig) {
		if base > 0 {
			c.retryBackoff = base
		}
	}
}

// WithExtraTransport envelopa um transporte interno customizado (proxy, TLS,
// overrides de dial). Default: cópia de http.DefaultTransport (clonada para
// nunca mutar o global).
func WithExtraTransport(rt http.RoundTripper) HTTPOption {
	return func(c *httpClientConfig) {
		c.extraTransport = rt
	}
}

// HTTPClient devolve um *http.Client com OpenTelemetry integrado no lado de
// SAÍDA (contraparte da factory do Middleware):
//
//   - tracing: propagação W3C (header traceparent) + span CLIENT por tentativa
//     via otelhttp.NewTransport(t.tp);
//   - retry com backoff que dobra a cada retry até 5s (+ jitter) sobre erros
//     transitórios (erro de rede e respostas 429/5xx), APENAS para métodos
//     idempotentes (GET, HEAD, PUT, DELETE) — POST/PATCH nunca são repetidos
//     (v1 simples: só métodos seguros podem repetir sem reproduzir efeitos);
//   - métricas POR TENTATIVA na família padrão http_client_* (ver
//     clientMetrics), reusando o meter adapter do Telemetry;
//   - timeout por tentativa (WithBaseTimeout); o Client não impõe Timeout
//     global — o prazo total é o ctx passado pelo caller na request;
//   - log WARN via slog quando TODAS as tentativas falham (correlacionado à
//     linhagem de ctx da request, via caminho logIn do Telemetry).
//
// Exemplo:
//
//	client := tel.HTTPClient(telemetry.WithBaseTimeout(5*time.Second))
//	err := tel.WithSpan("sync-upstream", func(ctx context.Context) error {
//		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
//		resp, err := client.Do(req) // traz traceparent automaticamente
//		if err != nil {
//			return err
//		}
//		defer resp.Body.Close()
//		...
//	})
func (t *Telemetry) HTTPClient(opts ...HTTPOption) *http.Client {
	cfg := defaultHTTPClientConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	inner := cfg.extraTransport
	if inner == nil {
		inner = cloneDefaultTransport()
	}

	// Propagação SEMPRE explícita (W3C TraceContext + Baggage): o propagador
	// global default do otel é NOOP — sem esta opção, apps com
	// RegisterGlobals=false fariam chamadas outbound SEM traceparent (o span
	// até nasceria, mas o header não seria injetado no request).
	httpOpts := []otelhttp.Option{
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		)),
	}
	// Igual ao Middleware: TracerProvider só explícito quando há tp.
	if t.tp != nil {
		httpOpts = append(httpOpts, otelhttp.WithTracerProvider(t.tp))
	}

	return &http.Client{
		Transport: &clientRetryTransport{
			next: otelhttp.NewTransport(inner, httpOpts...),
			cfg:  cfg,
			m:    newClientMetrics(t.Meter),
			tel:  t,
		},
	}
}

// cloneDefaultTransport devolve uma cópia do transporte padrão global para
// uso como base interna (evita mutar http.DefaultTransport).
func cloneDefaultTransport() http.RoundTripper {
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		return tr.Clone()
	}
	return http.DefaultTransport
}

// newClientMetrics cria as métricas do client no meter informado. Nil-safe
// (Telemetry construído manualmente sem meter ⇒ métricas silenciadas).
func newClientMetrics(m Meter) *clientMetrics {
	if m == nil {
		return &clientMetrics{}
	}
	cm := &clientMetrics{}
	cm.requestsTotal, _ = m.Counter("http_client_requests_total")
	cm.requestDuration, _ = m.Float64Histogram(
		"http_client_request_duration_seconds",
		metric.WithDescription("Duração de requests HTTP de saída em segundos"),
		metric.WithExplicitBucketBoundaries(latencyBucketBoundaries...),
		metric.WithUnit("s"),
	)
	cm.requestsInflight, _ = m.Int64UpDownCounter(
		"http_client_requests_inflight",
		metric.WithDescription("Requests HTTP de saída em voo"),
	)
	return cm
}

// retriableMethods são os métodos considerados seguros para repetição
// (idempotentes). POST/PATCH ficam fora por definição (v1 simples: só métodos
// seguros fazem retry).
var retriableMethods = map[string]struct{}{
	http.MethodGet:    {},
	http.MethodHead:   {},
	http.MethodPut:    {},
	http.MethodDelete: {},
}

// isRetriableMethod informa se o método participa do mecanismo de retry.
func isRetriableMethod(method string) bool {
	_, ok := retriableMethods[method]
	return ok
}

// isRetryableStatus considera transitório: 429 (rate limit) e toda a classe
// 5xx. Simplificação v1 assumindo idempotência já garantida pelo método.
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// outcomeLabel classifica UMA tentativa p/ o histograma: retry (vai repetir),
// success (resposta definitiva boa), error (falha definitiva — último erro,
// status final ruim ou resposta ≥400 sem mais tentativas disponíveis).
func outcomeLabel(willRetry bool, err error, statusCode int) string {
	switch {
	case willRetry:
		return "retry"
	case err != nil, statusCode >= 400:
		return "error"
	default:
		return "success"
	}
}

// retryDelay calcula o delay antes do próximo retry: base dobrada `attempt`
// vezes com jitter ±retryJitterFraction, SEMPRE limitado ao teto
// maxHTTPRetryBackoff (aplicado após o jitter).
func retryDelay(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 0; i < attempt && d < maxHTTPRetryBackoff; i++ {
		d *= 2
	}
	// Jitter de suavização de carga — sem propósito criptográfico.
	jittered := time.Duration(float64(d) * (1 - retryJitterFraction + rand.Float64()*2*retryJitterFraction)) // #nosec G404
	if jittered > maxHTTPRetryBackoff || jittered <= 0 {
		return maxHTTPRetryBackoff
	}
	return jittered
}

// attemptResult carrega o resultado de UMA tentativa pelo pipeline
// retry/métricas/log. O cancel fica retido porque o corpo da resposta segue
// vinculado ao ctx da tentativa após RoundTrip retornar (streaming); ele é
// invocado ao sobrepor a tentativa e em qualquer falha final.
type attemptResult struct {
	resp     *http.Response
	err      error
	status   int                // 0 quando err != nil
	elapsed  time.Duration      // duração da passagem pelo transporte
	ctx      context.Context    // ctx da tentativa (linhagem p/ métricas)
	cancel   context.CancelFunc // cancel do deadline desta tentativa
	attempts int                // nº total de tentativas executadas (preenchido no fim)
	fatal    bool               // erro que NÃO pode ser retentado (ex.: falha ao reconstruir corpo)
}

// clientRetryTransport envelopa o transporte instrumentado (otelhttp) com
// retry/backoff de métodos idempotentes, métricas por tentativa e log de
// falha final. Implementa http.RoundTripper (uso interno da factory HTTPClient).
type clientRetryTransport struct {
	next http.RoundTripper
	cfg  httpClientConfig
	m    *clientMetrics
	tel  *Telemetry
}

// Compile-time: o transporte do client é um RoundTripper válido.
var _ http.RoundTripper = (*clientRetryTransport)(nil)

// RoundTrip executa a cadeia de tentativas (métodos idempotentes) ou uma
// única passagem (POST/PATCH/etc. ou corpo não-retornável), gravando métricas
// por tentativa e logando falha final. O ctx da REQUEST (linhagem do caller)
// controla o ciclo todo: deadline/cancelamento propagam imediatamente.
func (rt *clientRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	res := rt.executeAttempts(req)

	// Log de falha FINAL (todas as tentativas esgotadas), correlacionado ao
	// ctx da request (herda trace_id da linhagem do caller nos sinks).
	if res.err != nil {
		rt.tel.logIn(req.Context(), slog.LevelWarn, "http client request failed",
			slog.String("method", req.Method),
			slog.String("host", targetHost(req)),
			slog.Int("attempts", res.attempts),
			slog.Duration("duration", time.Since(start)),
			slog.String("error", res.err.Error()),
		)
	}
	return res.resp, res.err
}

// executeAttempts roda o loop de tentativas com backoff e retorna o resultado
// FINAL. Sempre preenche attempts e devolve res.err em fracasso — inclusive
// quando o caller cancela durante o backoff (propaga req.Context().Err(),
// nunca resp=nil+err=nil).
func (rt *clientRetryTransport) executeAttempts(req *http.Request) attemptResult {
	reqAttrs := []attribute.KeyValue{
		attribute.String("method", req.Method),
		attribute.String("host", targetHost(req)),
	}

	// Retry somente se: método idempotente E corpo retornável (GetBody) para
	// poder reenviar. Caso contrário vira tentativa única (maxRetries=0).
	maxRetries := rt.cfg.maxRetries
	if !isRetriableMethod(req.Method) || (req.Body != nil && req.GetBody == nil) {
		maxRetries = 0
	}

	var pendingCancel context.CancelFunc // cancel da tentativa anterior (liberado no INÍCIO da iteração seguinte)
	for attempt := 0; ; attempt++ {
		if pendingCancel != nil {
			pendingCancel()
		}

		res := rt.tryOnce(req, reqAttrs, attempt)

		willRetry := rt.shouldRetry(req, res, attempt, maxRetries)
		rt.recordAttempt(res, reqAttrs, willRetry)

		if !willRetry {
			// Sucesso: NÃO chamamos o cancel da última tentativa durante o
			// RoundTrip — o body da resposta continua atrelado ao ctx dela
			// (streaming); o deadline vence naturalmente (limitado por
			// baseTimeout), troco documentado de wrappers de retry.
			// Fracasso: solta o deadline imediatamente.
			if res.err != nil {
				res.cancel()
			}
			res.attempts = attempt + 1
			return res
		}

		// Vai repetir: descarta corpo/resposta parcial antes do backoff e
		// retém o cancel desta tentativa p/ liberar na próxima iteração
		// (ou no cancelamento do caller durante a espera).
		drainResponse(&res)
		pendingCancel = res.cancel

		select {
		case <-time.After(retryDelay(rt.cfg.retryBackoff, attempt)):
		case <-req.Context().Done():
			// Caller desistiu durante o backoff: encerra propagaando o motivo
			// real — nunca resposta vazia com err=nil.
			pendingCancel()
			res.attempts = attempt + 1
			if res.err == nil {
				res.err = req.Context().Err()
			}
			return res
		}
	}
}

// shouldRetry decide se Esta tentativa ganha outra: método idempotente com
// espaço no orçamento, ctx do caller vivo e falha transitória (erro de rede
// ou resposta 429/5xx). Erros fatais (res.corpo irrecuperável) nunca repetem.
func (rt *clientRetryTransport) shouldRetry(req *http.Request, res attemptResult, attempt, maxRetries int) bool {
	if res.fatal || attempt >= maxRetries || req.Context().Err() != nil {
		return false
	}
	switch {
	case res.err != nil:
		return true // erro de transporte com ctx do caller ainda vivo
	case res.status > 0:
		return isRetryableStatus(res.status)
	default:
		return false
	}
}

// tryOnce executa UMA passagem pelo transporte instrumentado com deadline
// próprio (cfg.baseTimeout derivado da linhagem da request). Emite inflight
// (+1/-1 pareados mesmo em erro) e carrega ctx/duração no resultado.
func (rt *clientRetryTransport) tryOnce(req *http.Request, reqAttrs []attribute.KeyValue, attempt int) attemptResult {
	// Prazo POR TENTATIVA derivado da linhagem do caller.
	actx, cancel := context.WithTimeout(req.Context(), rt.cfg.baseTimeout)

	if rt.m != nil && rt.m.requestsInflight != nil {
		rt.m.requestsInflight.Add(actx, 1, metric.WithAttributes(reqAttrs...))
	}

	areq := req.Clone(actx)
	if attempt > 0 && req.Body != nil {
		// Recomeça o corpo para a nova tentativa (o original já foi consumido;
		// alcançável apenas quando GetBody existe — ver executeAttempts).
		b, berr := req.GetBody()
		if berr != nil {
			rt.decrementInflight(actx, reqAttrs)
			return attemptResult{err: berr, ctx: actx, cancel: cancel, attempts: attempt + 1, fatal: true}
		}
		areq.Body = b
	}

	start := time.Now()
	// nolint:bodyclose // transporte interno puro: o corpo é gerenciado AQUI —
	// devolvido ao caller (sucesso) ou drenado/fechado antes de cada retry.
	resp, err := rt.next.RoundTrip(areq)
	elapsed := time.Since(start)

	res := attemptResult{
		resp:     resp,
		err:      err,
		elapsed:  elapsed,
		ctx:      actx,
		cancel:   cancel,
		attempts: attempt + 1,
	}
	if err == nil && resp != nil {
		res.status = resp.StatusCode
	}
	rt.decrementInflight(actx, reqAttrs)
	return res
}

// decrementInflight decrementa o gauge inflight (nil-safe e defensivo contra
// double-decrement: cada chamador cai aqui uma única vez por tentativa).
func (rt *clientRetryTransport) decrementInflight(ctx context.Context, reqAttrs []attribute.KeyValue) {
	if rt.m != nil && rt.m.requestsInflight != nil {
		rt.m.requestsInflight.Add(ctx, -1, metric.WithAttributes(reqAttrs...))
	}
}

// recordAttempt emite as métricas de UMA tentativa: requests_total{status} e
// request_duration_seconds{outcome}, ambos sob o ctx DA TENTATIVA (mantém a
// correlação com o span CLIENT emitido pelo otelhttp). Nil-safe sem meter.
func (rt *clientRetryTransport) recordAttempt(res attemptResult, reqAttrs []attribute.KeyValue, willRetry bool) {
	if rt.m == nil {
		return
	}
	outcome := outcomeLabel(willRetry, res.err, res.status)

	statusAttrs := append(append([]attribute.KeyValue{}, reqAttrs...),
		attribute.Int("status", res.status))
	if rt.m.requestsTotal != nil {
		rt.m.requestsTotal.Add(res.ctx, 1, metric.WithAttributes(statusAttrs...))
	}

	outcomeAttrs := append(append([]attribute.KeyValue{}, reqAttrs...),
		attribute.String("outcome", outcome))
	if rt.m.requestDuration != nil {
		rt.m.requestDuration.Record(res.ctx, res.elapsed.Seconds(), metric.WithAttributes(outcomeAttrs...))
	}
}

// drainResponse fecha o corpo de uma tentativa descartada (leitura limitada),
// permitindo o reuso da conexão TCP pelo pool quando possível.
func drainResponse(res *attemptResult) {
	if res.resp == nil || res.resp.Body == nil {
		return
	}
	body := res.resp.Body
	res.resp = nil
	defer func() { _ = body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10))
}

// targetHost devolve host[:port] da URL da request ("" quando ausente).
func targetHost(req *http.Request) string {
	if req.URL != nil {
		return req.URL.Host
	}
	return ""
}
