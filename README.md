# hellnet-lib-telemetry

Opinionated OpenTelemetry observability library for Go services — traces,
metrics and logs out of the box, in the spirit of .NET `prometheus-net`
(automatic runtime/process metrics, HTTP, DB, workers, error rate).

All three signals are exported via **OTLP over HTTP** (`otlploghttp` /
`otlpmetrichttp` / `otlptracehttp`). A Prometheus `/metrics` scrape endpoint is
also available locally for inspection (no collector required).

---

## 🧒 Entenda com 15 anos

### A analogia

Um app **sem telemetria** é pilotar um avião com o **para-brisa pintado**: o avião voa
normalmente, mas quando algo despenca você não enxerga nada lá fora.

Telemetria é a **torre de controle** + um painelzinho de instrumentos na sua frente:

- **Logs** — o diário de bordo: "aconteceu X às 10h".
- **Metrics** — o velocímetro: quantas req/s, quanta memória em uso.
- **Traces** — o GPS do pedido: passou pela cozinha → caixa → entrega, e em que etapa demorou?
- **Health endpoints** — o "tá tudo bem?" periódico: `/live`, `/ready`, `/health`.

### O problema que resolve

- **Sem telemetria:** dá ruim às **2h da manhã** e ninguém tem pistas — só se sabe que "não funciona".
- **Com telemetria:** você vê exatamente **qual etapa quebrou**, o que ela registrou antes de cair
  e há quanto tempo aquilo vinha piorando.
- **Sem lib opinada:** plugar o OpenTelemetry peça por peça na mão é um projeto — **com ela**, ligar tudo
  é **uma linha de setup** (`telemetry.New`).

### Mini-dicionário

| Termo | Analogia |
|---|---|
| **log** | Linha escrita no diário de bordo: "aconteceu X às 10h" |
| **métrica** | Número do velocímetro: req/s, memória, duração — somado ao longo do tempo |
| **trace/span** | Trecho cronometrado da viagem; um **span filho** é a etapa dentro da etapa |
| **OTLP/collector** | A central que recebe os relatórios de todas as torres |
| **middleware** | O porteiro que anota quem chegou antes de qualquer coisa acontecer |
| **healthcheck** | A pergunta "tá tudo bem?", respondida por `/live`, `/ready` e `/health` |
| **baseCtx** | O mapa-múndi da aplicação: você entrega o contexto UMA vez no `New` e todos os relatórios herdam dele |

### Primeiras linhas

```go
ctx := context.Background() // contexto entregue UMA vez — todos os relatórios herdam dele

tel, err := telemetry.New(ctx) // valida HELLNET_* obrigatórias
defer func() { _ = tel.Shutdown() }() // desliga na ordem certa, sem perder relatórios
mux.Handle("/", telemetry.Middleware(tel, meuHandler)) // o porteiro anota cada request
```

As próximas seções mostram o detalhe técnico completo de cada peça.

---

## Quick start

```go
package main

import (
	"context"
	"net/http"

	"github.com/guilhermelinosp/hellnet-lib-telemetry/telemetry"
)

func main() {
	// Contexto da aplicação passado UMA vez na construção — tudo que a lib
	// executar (spans, workers, health checks, logs) herda dele.
	ctx := context.Background()

	tel, err := telemetry.New(ctx) // lê env HELLNET_TELEMETRY_* / HELLNET_*
	if err != nil {
		panic(err)
	}
	defer tel.Shutdown()

	mux := http.NewServeMux()
	mux.Handle("GET /live", tel.Live())
	mux.Handle("GET /ready", tel.Ready())
	mux.Handle("GET /health", tel.Health())
	mux.Handle("GET /metrics", tel.MetricsHandler()) // Prometheus scrape (opcional)

	http.ListenAndServe(":8080", telemetry.Middleware(tel, mux))
}
```

---

## Required environment variables

A lib aceita o prefixo **`HELLNET_TELEMETRY_*`** (padrão hellnet) ou o antigo
**`HELLNET_*`** (fallback de retrocompatibilidade). Ambos funcionam.

| Variable | Example | Description |
|---|---|---|
| `HELLNET_TELEMETRY_SERVICE` | `order-api` | Service identifier (required) |
| `HELLNET_TELEMETRY_ENDPOINT` | `http://alloy.monitoring:4318` | OTLP collector endpoint (required). **A porta deve vir junto do endpoint** (ex.: `:4318` ou `:443`); não há variável de porta separada. Se a porta for omitida, é inferida do scheme (443 p/ https, 80 p/ http) |
| `HELLNET_TELEMETRY_ENVIRONMENT` | `Development` | Ambiente (**opcional**); controla `.env` loading. Ausente = tratado como dev |

> Apenas `SERVICE` e `ENDPOINT` são obrigatórios. A porta **não** é configurável via env separada — ela vive no `ENDPOINT`.
> Arquivo `.env` explícito: `HELLNET_TELEMETRY_ENV_FILE` (ou `HELLNET_ENV_FILE`).

---

## Configuration

### Default (from env)

```go
ctx := context.Background()

// env-first (sem Options): carrega .env + HELLNET_TELEMETRY_* / HELLNET_*
tel, _ := telemetry.New(ctx)
```

### Custom options

```go
opts := telemetry.Options{
	ServiceName:   "my-service",
	OTLPEndpoint:  "http://collector:4318",
	LogLevel:      slog.LevelDebug,
	Enabled:       true, // liga trace + metrics + logs de uma vez
	ResourceAttrs: []attribute.KeyValue{
		attribute.String("deployment.region", "us-east-1"),
	},
	PrometheusExporter: true, // expõe /metrics (default true)
}
tel, _ := telemetry.New(ctx, opts)
```

### Application context (passado UMA vez)

O contexto da aplicação entra **somente no `New`/`MustNew`** e fica retido
internamente (`baseCtx`). Nenhum método da lib recebe ctx de app. É o único
lugar para anexar baggage/propagators OTel de longa duração.

Consequência de correlação: traces de aplicação formam **uma única linhagem**
com raiz no `baseCtx` (`WithSpan`/`Worker` criam filhos sob ele; o ctx derivado
é repassado ao callback para continuação por código otel-instrumentado).
Traces request-scoped extraídos pelo **Middleware** permanecem independentes —
origem são os requests inbound (comportamento server-side correto).

### Tudo ligado ou tudo desligado

`Enabled` controla os três sinais juntos. Não há toggle individual. Para
desabilitar tudo (ex.: testes unitários), use `Enabled: false`.

```go
opts := telemetry.Options{
	ServiceName:  "my-service",
	OTLPEndpoint: "http://collector:4318",
	Enabled:      false, // sem trace/metrics/logs
}
```

> **Globais (opt-in):** `Options.RegisterGlobals` (default `false`) controla se os
> providers são registrados no estado global do otel/slog. Deixe `false` e use
> `tel.*` / `telemetry.Client` para DI/mock limpa; `true` reproduz o comportamento
> legado.

---

## Health endpoints

> 🧒 **Entenda com 15 anos:** perguntar "tudo bem?" a cada instante — `/live`, `/ready` e `/health` respondem.

| Endpoint | Handler | Purpose |
|---|---|---|
| `GET /live` | `tel.Live()` | Liveness probe — always 200 |
| `GET /ready` | `tel.Ready()` | Readiness — self + OTLP collector TCP dial |
| `GET /health` | `tel.Health()` | Aggregate — `ok`/`degraded` with all checks |

```go
mux.Handle("GET /live", tel.Live())
mux.Handle("GET /ready", tel.Ready())
mux.Handle("GET /health", tel.Health())
```

**Response format:**

```json
{
  "status": "ready",
  "checks": [
    {"name": "self", "status": "pass"},
    {"name": "otlp-collector", "status": "pass"}
  ]
}
```

### Custom health checks

Além de `self` e do collector OTLP, registre dependências (DB, redis,
downstream). Qualquer falha marca o serviço `not ready` / `degraded`:

```go
tel.HealthRegister("postgres", func(ctx context.Context) error {
	return db.PingContext(ctx)
})
```

Métricas produzidas: `healthcheck_status{check,status}`,
`healthcheck_duration_seconds{check}`, `healthcheck_all_pass`.

---

## Tracing

> 🧒 **Entenda com 15 anos:** GPS etapa-por-etapa — dá pra ver por onde o pedido passou e onde demorou.

Fluxo padrão (ctx-free — span derivado do contexto-base, repassado ao callback):

```go
err := tel.WithSpan("process-order", func(ctx context.Context) error {
	// ctx contém o span; código otel-instrumentado continua a linhagem
	return process(ctx, order)
})
// em erro: span marcado com status=Error + RecordError
```

> `WithSpan` **recupera panics**: marca o span como erro, incrementa
> `exceptions_total{span,kind=panic}` e **re-propaga o panic** (comportamento
> original preservado).

### Escape hatch avançado (`Trace().Start`)

Precisa enraizar um span num ctx próprio? Use a superfície explícita com ctx
(documentada como interna/avançada; fora do fluxo padrão de correlação):

```go
ctx, span := tel.Trace().Start(parentCtx, "operation-name",
	trace.WithAttributes(attribute.String("order.id", "123")))
defer span.End()

span.AddEvent("validation-started")
span.SetAttributes(attribute.Int("items.count", 5))
```

---

## Metrics

> 🧒 **Entenda com 15 anos:** velocímetro/painel do carro — quanto trafega por segundo, quanta memória gasta.

A lib já coleta dezenas de métricas **automaticamente** (sem código seu). Há
três formas de métricas:

1. **Automáticas** (HTTP, runtime/processo, erros, DB, health, workers) — veja
   catálogo abaixo.
2. **Instrumentação de cliente HTTP** (`tel.HTTPClient`) — automática ao usar o
   `http.Client` retornado.
3. **Customizadas** — crie contadores/histogramas/gauges via `tel.Meter` (ou
   `tel.Metrics()`).

### Custom metrics

```go
// Counter (atalho int64, sem opts)
requestsTotal, _ := tel.Meter.Counter("http.requests.total")
requestsTotal.Add(ctx, 1, metric.WithAttributes(
	attribute.String("method", "GET"),
	attribute.String("path", "/api/users"),
))

// Histogram
requestDuration, _ := tel.Meter.Float64Histogram("http.request.duration")
requestDuration.Record(ctx, 0.123, metric.WithAttributes(
	attribute.String("method", "GET"),
))

// Observable Gauge (callback-based)
activeConns, _ := tel.Meter.Int64ObservableGauge("http.connections.active")
tel.Meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
	o.ObserveInt64(activeConns, getActiveCount())
	return nil
}, activeConns)

// Gauge (atalho int64)
queueGauge, _ := tel.Meter.Gauge("queue.depth")
queueGauge.Record(ctx, int64(q))
hist, _ := tel.Meter.Histogram("http_duration_ms")
defer func(start time.Time) { hist.Record(ctx, time.Since(start).Milliseconds()) }(time.Now())
```

### Prometheus `/metrics` endpoint

`PrometheusExporter` (default `true`) anexa um exporter Prometheus à **mesma**
`MeterProvider` usada pelo OTLP. Exponha `tel.MetricsHandler()` (um
`http.Handler`) em qualquer rota para inspecionar everything localmente:

```go
mux.Handle("GET /metrics", tel.MetricsHandler())
```

O que aparece em `/metrics` é exatamente o que é enviado ao OTLP (mesma
agregação do SDK, dois readers no mesmo provider).

### Catálogo de métricas automáticas

**HTTP — servidor (Middleware)** `{method,status}`:

- `http_requests_total` — contagem de requests
- `http_request_duration_seconds` — histograma de duração (buckets de latência)
- `http_requests_inflight` — concorrência ativa (UpDownCounter)
- `http_response_size_bytes` — histograma do tamanho da resposta
- `http_requests_body_size_bytes` — histograma do tamanho do corpo da requisição (via `Content-Length`)
- `http_server_errors_total{method}` — respostas com status ≥ 400 (taxa de erro)

**HTTP — cliente (`tel.HTTPClient`)** `{method,status,host[,outcome]}`:

- `http_client_requests_total` — total de tentativas de saída
- `http_client_request_duration_seconds{outcome=success|retry|error}` — histograma por tentativa
- `http_client_requests_inflight` — chamadas de saída em voo

**Workers (`tel.Worker`)** `{job,status[,extra]}`:

- `worker_jobs_total` — total de execuções (ok/error)
- `worker_job_duration_seconds` — histograma → p99/p95/p50
- `worker_jobs_inflight` — concorrência ativa

**Database (`tel.WatchDB(db, name)`)** `{db}`:

- `db_sql_open_connections`, `db_sql_in_use_connections`, `db_sql_idle_connections`,
  `db_sql_max_open_connections`
- `db_sql_wait_count_total`, `db_sql_wait_duration_seconds_total`
- `db_sql_closed_max_lifetime_total`, `db_sql_closed_max_idle_total`

**Health:**

- `healthcheck_status{check,status}`, `healthcheck_duration_seconds{check}`,
  `healthcheck_all_pass`

**Erros / exceções (automáticas):**

- `log_errors_total{level}` — qualquer log slog com nível ≥ Error (via handler
  interno; basta usar `tel.Logger.Error(...)`)
- `http_server_errors_total{method}` — respostas HTTP com status ≥ 400
- `exceptions_total{span,kind}` — panics recuperados em `WithSpan`

**Runtime / processo (LIGADO POR PADRÃO quando metrics habilitado):**

Conjunto clássico (SDK observables):

- Memória (bytes): `process_goroutines`, `process_heap_alloc_bytes`,
  `process_heap_sys_bytes`, `process_heap_inuse_bytes`, `process_heap_released_bytes`,
  `process_heap_objects`, `process_stack_inuse_bytes`, `process_stack_sys_bytes`,
  `process_mspan_inuse_bytes`, `process_mspan_sys_bytes`, `process_mcache_inuse_bytes`,
  `process_mcache_sys_bytes`, `process_other_sys_bytes`, `process_gc_sys_bytes`,
  `process_sys_bytes`, `process_total_alloc_bytes`
- GC: `process_gc_total`, `process_gc_forced_total`, `process_gc_pause_total_seconds`,
  `process_gc_cpu_fraction`, `process_gc_pause_seconds` (histograma → p99 da pausa)
- CPU/geral: `process_cpu_usage_percent`, `process_cpu_usage_ratio`, `process_num_cpu`,
  `process_uptime_seconds`, `process_open_fds` *(Linux)*, `process_threads` *(Linux)*

Conjunto detalhado (`runtime/metrics`, nível prometheus-net):

- Memória: `process_heap_live_bytes`, `process_heap_free_bytes`,
  `process_gc_heap_goal_bytes`, `process_gc_heap_limit_bytes`,
  `process_mem_heap_objects_bytes`, `process_mem_heap_stacks_bytes`,
  `process_mem_metadata_mcache_free_bytes`, `process_mem_metadata_mcache_inuse_bytes`,
  `process_mem_metadata_other_bytes`, `process_mem_os_stacks_bytes`,
  `process_mem_other_bytes`, `process_mem_profiling_buckets_bytes`
- Mutex: `process_mutex_wait_seconds_total`, `process_mutex_lock_seconds_total`
- CPU por classe (segundos): `process_cpu_gc_seconds_total`,
  `process_cpu_gc_mark_assist_seconds_total`, `process_cpu_gc_mark_dedicated_seconds_total`,
  `process_cpu_gc_mark_idle_seconds_total`, `process_cpu_gc_sweep_assist_seconds_total`,
  `process_cpu_gc_sweep_dedicated_seconds_total`, `process_cpu_gc_sweep_idle_seconds_total`,
  `process_cpu_scavenge_seconds_total`, `process_cpu_total_seconds_total`,
  `process_cpu_user_seconds_total`, `process_cpu_idle_seconds_total`

> ⚠️ `process_cpu_usage_percent`, `process_cpu_usage_ratio`, `process_open_fds` e
> `process_threads` dependem de `/proc` e **só são emitidos em Linux**. Em macOS
> (dev) elas ficam ausentes; aparecem normalmente no deploy Linux.

Desligue com `opts.RuntimeMetrics = false`.

### Exemplo de queries (PromQL)

```promql
# Taxa de requests (servidor)
sum(rate(http_requests_total[5m])) by (method, status)

# p99 / p95 / p50 de latência (servidor)
histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket[5m])) by (le, method))
histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket[5m])) by (le, method))
histogram_quantile(0.50, sum(rate(http_request_duration_seconds_bucket[5m])) by (le, method))

# Taxa de erro do servidor (4xx+5xx)
sum(rate(http_server_errors_total[5m])) by (method)
  / sum(rate(http_requests_total[5m])) by (method)

# Erros logados e exceções
sum(rate(log_errors_total[5m])) by (level)
sum(rate(exceptions_total[5m])) by (span, kind)

# Throughput / p99 de worker
sum(rate(worker_jobs_total[5m])) by (job, status)
histogram_quantile(0.99, sum(rate(worker_job_duration_seconds_bucket[5m])) by (le, job))

# Pool de DB
db_sql_in_use_connections / db_sql_max_open_connections
rate(db_sql_wait_count_total[5m])

# p99 da PAUSA de GC (não do tempo total)
histogram_quantile(0.99, rate(process_gc_pause_seconds_bucket[5m]))

# CPU do processo (% de 1 core; >100 em multi-core)
avg(process_cpu_usage_percent)
```

> ⚠️ **Pré-requisito para `histogram_quantile`**: o histograma precisa chegar no
> Prometheus como **histograma clássico** (série `_bucket`). Se o coletor exportar
> como native/exponential histogram, habilite native histograms no Prometheus 3.x
> (ou force histograma clássico no exporter de Prometheus).

### Por que não há métrica `p99` pronta?

No OpenTelemetry percentis **não são emitidos** — o que sai é um histograma
(buckets + `_sum` + `_count`). O p99/p95 é calculado **na consulta** com
`histogram_quantile`. Exportar percentis pré-computados seria um anti-pattern
(somar percentis é matematicamente inválido).

---

## Logging

> 🧒 **Entenda com 15 anos:** diário de bordo — "às 10h03 aconteceu X", registrado na hora.

Usa `log/slog` com saída dupla: **stdout (JSON)** + **OTLP → Loki**.

```go
tel.Log().Info("order created",
	"order_id", "123", "customer_id", "456", "amount", 99.90)

tel.Log().Warn("rate limit approaching", "current", 95, "limit", 100)

tel.Log().Error("payment failed", "order_id", "123", "error", err)

// Debug só aparece se LogLevel=Debug
tel.Log().Debug("cache hit", "key", "user:123")
```

> A abstração `Log()` não recebe ctx: a correlação trace→log usa o
> contexto-base internamente. Para correlação request-scoped, os logs do
> Middleware já fazem isso automaticamente (via span do request). O campo cru
> `tel.Logger` (*slog.Logger) continua disponível para quem precisa de ctx.

**stdout:**

```json
{"time":"2026-01-15T10:30:00.123Z","level":"INFO","msg":"order created","order_id":"123","customer_id":"456","amount":99.9}
```

### Redação de logs (PII)

Mascara valores de atributos sensíveis (`password`, `token`, `secret`,
`authorization`, `api_key`, ...) no stdout **e** no sink OTLP:

```go
opts.RedactSensitive = true
// ou chaves customizadas:
opts.RedactKeys = []string{"session_id"}
```

---

## HTTP Middleware

Envolve qualquer `http.Handler` com:

- **OTel tracing** (via `otelhttp`)
- **Métricas de request** (as métricas `http_*` acima)
- **Request logging** estruturado (method, path, host, status, duration)

```go
mux := http.NewServeMux()
mux.Handle("GET /api/users", handler)

server := http.Server{
	Addr:    ":8080",
	Handler: telemetry.Middleware(tel, mux),
}
server.ListenAndServe()
```

**Log por request:**

```json
{"time":"...","level":"INFO","msg":"request completed","method":"GET","path":"/api/users","host":"localhost:8080","status":200,"duration":12.345678}
```

### HTTP client instrumentado

> 🧒 **Entenda com 15 anos:** o mensageiro que leva o crachá do trace junto —
> e, se a porta estiver ocupada, bate de novo com educação antes de desistir.

`tel.HTTPClient(opts ...HTTPOption) *http.Client` cria um client de SAÍDA com
OpenTelemetry completo (contraparte do Middleware server-side):

- **Propagação W3C**: header `traceparent` injetado automaticamente — o serviço
  downstream continua o MESMO trace (span CLIENT por tentativa, filho do span
  ativo no seu código);
- **Retry com backoff** (dobra até 5s, jitter ±20%) em erros transitórios
  (rede + 429/5xx), apenas para métodos idempotentes (`GET`, `HEAD`, `PUT`,
  `DELETE`) — `POST`/`PATCH` **nunca** repetem;
- **Timeout por tentativa** via `WithBaseTimeout` (o prazo total é o `ctx`
  que você passa na request);
- **Métricas por tentativa**: `http_client_requests_total{method,host,status}`,
  `http_client_request_duration_seconds{method,host,outcome}`,
  `http_client_requests_inflight`;
- Log WARN automático quando todas as tentativas falham.

```go
client := tel.HTTPClient(
	telemetry.WithBaseTimeout(5*time.Second),
	telemetry.WithMaxRetries(2),
)

err := tel.WithSpan("sync-upstream", func(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.example.com/orders", nil)
	resp, err := client.Do(req) // traz traceparent automaticamente
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return process(resp.Body)
})
```

| Opção | Default | O que faz |
|---|---|---|
| `WithBaseTimeout(d)` | `10s` | Timeout **por tentativa** (inclui ler o body) |
| `WithMaxRetries(n)` | `2` | Retries extras; tentativas = n+1; `0` desliga |
| `WithRetryBackoff(base)` | `100ms` | Delay inicial entre tentativas (dobra até 5s, jitter ±20%) |
| `WithExtraTransport(rt)` | clone de `DefaultTransport` | Transporte interno customizado (proxy/TLS/dial) |

> A propagação W3C é sempre ligada na factory (independe de
> `RegisterGlobals`) — apps com DI/testes continuam propagando o trace.

---

## Workers (background jobs, queues, cron)

> 🧒 **Entenda com 15 anos:** robô que trabalha enquanto você dorme — e cada turno dele vem com crachá rastreado.

`Worker` é a versão "sem HTTP" do `Middleware`: qualquer unidade de trabalho ganha
observabilidade automática (trace + métricas + log) sem boilerplate. O erro de
`fn` é repassado, então o caller decide retry/backoff.

```go
err := tel.Worker("process_order",
	func(ctx context.Context) error {
		return process(ctx, msg)
	},
	attribute.String("queue", "orders"), // atributos extras p/ quebra de séries
)
if err != nil {
	// decidir retry/backoff
}
```

Métricas: `worker_jobs_total{job,status[,queue]}`,
`worker_job_duration_seconds{job,status}`, `worker_jobs_inflight{job[,queue]}`.

---

## Database (pool metrics)

`tel.WatchDB(db, name)` registra métricas automáticas do pool de conexões via
callback (lê `db.Stats()` a cada exportação — sem goroutines próprias):

```go
db, _ := sql.Open("pgx", dsn)
tel.WatchDB(db, "main") // "replica", etc.
```

Métricas: `db_sql_*` (veja catálogo acima), particionadas por `db=name`.

---

## Development mode (.env loading)

Carrega `.env` **somente em Development**:

```go
func main() {
	telemetry.Loading() // carrega .env (dev/unset) + valida HELLNET_*; pula em Production/Staging

	tel, _ := telemetry.New(context.Background())
}
```

- Verifica `HELLNET_ENVIRONMENT` (obrigatório)
- Só carrega se valor for `Development`/`development`
- Usa `HELLNET_ENV_FILE` ou o `.env` do **mesmo diretório do executável**
  (onde `main` reside) — resolve via `os.Executable()`. Se não houver `.env`
  ao lado do binário, faz fallback para o diretório corrente (`os.Getwd()`).
- **Não sobrescreve** env vars existentes

---

## Shutdown

> 🧒 **Entenda com 15 anos:** desligar na ordem certa pra não perder relatórios.

Sempre chame para flush dos buffers:

```go
defer tel.Shutdown() // timeout interno de 5s; força flush OTLP + Prometheus
```

---

## Abstração (`Client` interface)

Para DI/mock, use a abstração em vez dos campos crus. O tipo **não aparece no
nome do método** — `int64`/`float64` é resolvido no acessor (`Int64()`/`Float64()`)
e os métodos são `Counter`/`Gauge`/`Histogram` agnósticos (genéricos).

```go
var c telemetry.Client = tel

// Metrics — tel.Meter expõe Counter/Gauge/Histogram (int64, nome sem tipo)
// + toda a superfície crua de metric.Meter (Float64*, Observable*, RegisterCallback).
counter, _ := c.Metrics().Counter("req_total")
counter.Add(ctx, 1)
c.Metrics().Gauge("queue").Record(ctx, int64(q))
c.Metrics().Histogram("latency").Record(ctx, d.Milliseconds())
c.Metrics().Float64Histogram("latency_s").Record(ctx, d.Seconds())

// também direto no tel (sem passar pelo Client):
tel.Meter.Counter("hellnet_smoke_ops_total")

// Traces (escape hatch avançado; fluxo padrão é tel.WithSpan)
_, span := c.Trace().Start(parentCtx, "order")
defer span.End()

// Logs (níveis padrão slog, sem ctx — correlação via contexto-base)
c.Log().Error("boom", "err", err)
c.Log().Info("started")
```

`*Telemetry` já satisfaz `telemetry.Client` (non-breaking). Quando metrics/logging/
tracing estão desligados, os acessores retornam implementações noop (nunca `nil`).

---

## API reference

| Function | Description |
|---|---|
| `telemetry.New(ctx, opts...)` | Setup all-in-one; **ctx da app passado UMA vez** (baseCtx interno) |
| `telemetry.MustNew(ctx, opts...)` | Como `New`, mas entra em pânico em erro |
| `telemetry.Loading()` | Carrega `.env` (dev) + valida envs |
| `telemetry.Middleware(tel, handler)` | HTTP tracing + request metrics + logging (request-scoped) |
| `tel.Live()` / `tel.Ready()` / `tel.Health()` | Health probes (`http.Handler`) |
| `tel.HealthRegister(name, fn)` | Custom health check — ctx **fornecido pela lib** |
| `tel.MetricsHandler()` | `http.Handler` Prometheus `/metrics` |
| `tel.WithSpan(name, fn)` | Span (raiz = baseCtx) + erro automático + `exceptions_total` em panic |
| `tel.Trace().Start(ctx, name)` | Escape hatch avançado: span enraizado num ctx próprio |
| `tel.Meter.Counter/Gauge/Histogram(name)` | Atalhos int64 de métrica |
| `tel.Log().Info/Error(...)` | Logging estruturado sem ctx (stdout + OTLP) |
| `tel.Worker(job, fn, extra...)` | Job/worker: span + `worker_*` metrics (ctx vem do baseCtx) |
| `tel.HTTPClient(opts...)` | `*http.Client` outbound: trace W3C + retry/backoff + métricas `http_client_*` |
| `tel.WatchDB(db, name)` | Métricas automáticas do pool SQL (`db_sql_*`) |
| `tel.Shutdown()` | Flush OTLP + Prometheus |
| `opts.RedactSensitive` / `opts.RedactKeys` | Mascara PII nos logs |
| `opts.RuntimeMetrics` | Liga/desliga runtime metrics (default ligado) |
| `opts.PrometheusExporter` | Liga `/metrics` (default `true`) |
| `opts.RegisterGlobals` | Registra providers no estado global otel/slog |

---

## Tech stack / Architecture

| Pillar | Library | Export |
|---|---|---|
| **Traces** | `go.opentelemetry.io/otel` + `otlptracehttp` | OTLP HTTP → Collector → Tempo |
| **Metrics** | `go.opentelemetry.io/otel` + `otlpmetrichttp` (+ `exporters/prometheus`) | OTLP HTTP → Collector → Prometheus; e `/metrics` local |
| **Logs** | `log/slog` + `otelslog` bridge | OTLP HTTP → Collector → Loki |

Todos os sinais usam **OTLP HTTP**. gRPC não é suportado na configuração atual.

Go 1.27+.

---

## Testing

```bash
go test ./telemetry/...
go test -race ./telemetry/...
```

## Desenvolvimento (Makefile)

O repositório segue o template de libs Go — use os targets do `Makefile`:

```bash
make all         # fmt + vet + lint + test
make fmt         # go fmt ./...
make vet         # go vet ./...
make lint        # golangci-lint run ./...
make test-race   # go test -race ./...
make cover       # cobertura (coverage.out)
make tidy        # go mod tidy
```

Hooks de git (Lefthook): `lefthook install` — pre-commit (fmt/vet/tidy/lint),
pre-push (`go test -race`), commit-msg (conventional commits).

Veja `example/main.go` para um serviço executável com todos os recursos.
