# hellnet-lib-telemetry

Opinionated OpenTelemetry observability library for Go services — traces,
metrics and logs out of the box.
(automatic runtime/process metrics, HTTP, DB, workers, error rate).

All three signals are exported via **OTLP over HTTP** (`otlploghttp` /
`otlpmetrichttp` / `otlptracehttp`). Não há endpoint local de scrape: métricas
são exportadas exclusivamente via OTLP.

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
| **context** | O vínculo da operação; APIs context-first preservam a linhagem distribuída |

### Primeiras linhas

```go
	tel, err := telemetry.New() // sem parâmetros: lê HELLNET_* e OTEL_*
defer func() { _ = tel.Close() }() // desliga na ordem certa, sem perder relatórios
mux.Handle("/", telemetry.Middleware(tel, meuHandler)) // o porteiro anota cada request
```

As próximas seções mostram o detalhe técnico completo de cada peça.

---

## Quick start

```go
package main

import (
	"net/http"

	"github.com/guilhermelinosp/hellnet-lib-telemetry/telemetry"
)

func main() {
	// Sem parâmetros: a lib lê HELLNET_TELEMETRY_* / HELLNET_* / OTEL_*.
	tel, err := telemetry.New()
	if err != nil {
		panic(err)
	}
	defer tel.Close()

	mux := http.NewServeMux()
	mux.Handle("GET /live", tel.Live())
	mux.Handle("GET /ready", tel.Ready())
	mux.Handle("GET /health", tel.Health())

	http.ListenAndServe(":8080", telemetry.Middleware(tel, mux))
}
```

---

## Required environment variables

A lib aceita **`HELLNET_TELEMETRY_*`**, o antigo **`HELLNET_*`** e os nomes
padrão **`OTEL_*`**, nessa ordem de precedência.

| Variable | Example | Description |
|---|---|---|
| `HELLNET_TELEMETRY_SERVICE` / `OTEL_SERVICE_NAME` | `order-api` | Service identifier (default `telemetry`) |
| `HELLNET_TELEMETRY_ENDPOINT` / `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://alloy.monitoring:4318` | OTLP collector endpoint (optional) |
| `HELLNET_TELEMETRY_ENVIRONMENT` | `Development` | Ambiente (**opcional**); usado como atributo de resource (`deployment.environment`) |

> O endpoint é opcional: vazio desliga a exportação OTLP, mantendo logs locais.

> **Endpoint vazio**: se `HELLNET_TELEMETRY_ENDPOINT` (ou `HELLNET_ENDPOINT`) não
> for definido, o export OTLP é desligado (logs ficam só em stdout; métricas e
> traces não exportam) — em vez de tentar exportar para uma URL inválida.`

---

## Configuration

### De ambiente (sem parâmetros)

`New()` **não recebe parâmetros**. Ela lê as variáveis `HELLNET_*` legadas e
as variáveis padrão do OpenTelemetry:

```go
tel, _ := telemetry.New()
```

Precedência: `HELLNET_TELEMETRY_*`, `HELLNET_*`, depois `OTEL_*`. Os headers
OTLP podem ser informados em `OTEL_EXPORTER_OTLP_HEADERS` ou em
`Options.OTLPHeaders` como `key=value,key2=value2`. Sampling usa
`OTEL_TRACES_SAMPLER` (`always_on`, `always_off` ou `traceidratio`) e
`OTEL_TRACES_SAMPLER_ARG`.

### Application context

Prefira APIs context-first para preservar a linhagem distribuída. As variantes
sem contexto continuam disponíveis para jobs sem contexto de entrada:

```go
err := tel.WithSpanContext(ctx, "process-order", func(ctx context.Context) error {
	return process(ctx, order)
})
tel.LogContext(ctx).Info("processing")
err = tel.WorkerContext(ctx, "reconcile", run)
```

### Sempre ligado

Os três sinais (trace + metrics + logs) estão **sempre ligados** por padrão.
Não há toggle para desligá-los:

```go
tel, _ := telemetry.New()
```

Os providers são registrados no estado global do otel; logs usam Zap e métricas
são exportadas exclusivamente via OTLP.

---

## Health endpoints

> 🧒 **Entenda com 15 anos:** perguntar "tudo bem?" a cada instante — `/live`, `/ready` e `/health` respondem.

| Endpoint | Handler | Purpose |
|---|---|---|
| `GET /live` | `tel.Live()` | Liveness probe — always 200 |
| `GET /ready` | `tel.Ready()` | Readiness — self + custom application checks |
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
    {"name": "postgres", "status": "pass"}
  ]
}
```

### Custom health checks

Registre dependências (DB, redis, downstream). `/ready` verifica apenas `self`
e os checks da aplicação; o collector OTLP é informativo em `/health` e não
remove o serviço da rotação:

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

Fluxo recomendado (context-first):

```go
err := tel.WithSpanContext(ctx, "process-order", func(ctx context.Context) error {
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
   `tel.Metric()`).

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

- `log_errors_total{level}` — qualquer log Zap com nível ≥ Error.
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

O conjunto acima é a única fonte customizada de métricas de runtime da lib;
ela não registra uma segunda instrumentação `runtime/metrics` em paralelo.

> ⚠️ `process_cpu_usage_percent`, `process_cpu_usage_ratio`, `process_open_fds` e
> `process_threads` dependem de `/proc` e **só são emitidos em Linux**. Em macOS
> (dev) elas ficam ausentes; aparecem normalmente no deploy Linux.

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
> O collector pode converter o histograma OTLP para o formato adequado ao
> backend de métricas utilizado.

### Por que não há métrica `p99` pronta?

No OpenTelemetry percentis **não são emitidos** — o que sai é um histograma
(buckets + `_sum` + `_count`). O p99/p95 é calculado **na consulta** com
`histogram_quantile`. Exportar percentis pré-computados seria um anti-pattern
(somar percentis é matematicamente inválido).

---

## Logging

> 🧒 **Entenda com 15 anos:** diário de bordo — "às 10h03 aconteceu X", registrado na hora.

Usa `go.uber.org/zap` com saída dupla: **stdout (JSON)** + **OTLP → Loki**.

```go
tel.Log().Info("order created",
	"order_id", "123", "customer_id", "456", "amount", 99.90)

tel.Log().Warn("rate limit approaching", "current", 95, "limit", 100)

tel.Log().Error("payment failed", "order_id", "123", "error", err)

// Debug só aparece se LogLevel=Debug
tel.Log().Debug("cache hit", "key", "user:123")

tel.Log().Trace("cache lookup detail", "key", "user:123")
tel.Log().Fatal("worker cannot continue", "worker", "billing")
tel.Log().Critical("data integrity failure", "table", "orders")
```

Níveis exportados: `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR`, `FATAL` e
`CRITICAL`. `Trace` exige `LogLevel <= -8`; `Debug` exige `LogLevel <= Debug`.
`Fatal` e `Critical` registram severidade alta, mas não encerram processo; o
bootstrap da aplicação decide quando terminar.

Para preservar correlação em código request-scoped, use:

```go
tel.LogContext(ctx).Error("request failed", "error", err)
```

`Log()` usa contexto-base. `LogContext(ctx)` preserva correlação request-scoped.
`tel.Logger` é um `*zap.SugaredLogger`.

**stdout:**

```json
{"time":"2026-01-15T10:30:00.123Z","level":"INFO","msg":"order created","order_id":"123","customer_id":"456","amount":99.9}
```

### Segurança

Não registre tokens, senhas ou PII nos argumentos de log. `/metrics` e
`/debug/pprof` não são criados automaticamente por esta biblioteca; monte
qualquer endpoint administrativo em listener protegido e separado da porta
pública da aplicação.

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

> A propagação W3C é sempre ligada na factory — apps com DI/testes continuam
> propagando o trace.

---

## Workers (background jobs, queues, cron)

> 🧒 **Entenda com 15 anos:** robô que trabalha enquanto você dorme — e cada turno dele vem com crachá rastreado.

`Worker` é a versão "sem HTTP" do `Middleware`: qualquer unidade de trabalho ganha
observabilidade automática (trace + métricas + log) sem boilerplate. O erro de
`fn` é repassado, então o caller decide retry/backoff.

## Resiliência

O pacote `resilience` fornece primitivas independentes de domínio para
compor chamadas externas e jobs:

- `Retry` — limite de tentativas, backoff exponencial, jitter e cancelamento
  por contexto;
- `CircuitBreaker` — estados `closed`, `open` e `half-open`, com limiar de
  falhas e janela de recuperação;
- `Bulkhead` — limite de concorrência com rejeição rápida quando cheio;
- `Fallback` e `Timeout` — degradação explícita e limite do orçamento da chamada;
- `Chain` e `Do[T]` — composição de políticas com resultado tipado.

Exemplo:

```go
breaker := &resilience.CircuitBreaker{
    Threshold:   5,
    OpenTimeout: 30 * time.Second,
}
policy := resilience.Chain(
    resilience.Fallback(func(ctx context.Context, err error) error {
        return serveFromCache(ctx, err)
    }),
    breaker.Policy(),
    resilience.Retry(resilience.RetryConfig{
        MaxAttempts: 3,
        BaseDelay:   100 * time.Millisecond,
        MaxDelay:    2 * time.Second,
        Jitter:      0.20,
        ShouldRetry: isTransient,
    }),
    resilience.Bulkhead(20),
    resilience.Timeout(2*time.Second),
)

order, err := resilience.Do(ctx, policy, func(ctx context.Context) (*Order, error) {
    return callDependency(ctx)
})
```

A ordem recomendada é `bulkhead → circuit breaker → retry → timeout →
operação`. A aplicação decide o fallback e a classificação de erros; a lib
não repete automaticamente erros de negócio.

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

## Close

> 🧒 **Entenda com 15 anos:** desligar na ordem certa pra não perder relatórios.

Sempre chame para flush dos buffers:

```go
defer tel.Close() // timeout interno de 5s; força flush OTLP
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
counter, _ := c.Metric().Counter("req_total")
counter.Add(ctx, 1)
c.Metric().Gauge("queue").Record(ctx, int64(q))
c.Metric().Histogram("latency").Record(ctx, d.Milliseconds())
c.Metric().Float64Histogram("latency_s").Record(ctx, d.Seconds())

// também direto no tel (sem passar pelo Client):
tel.Meter.Counter("hellnet_smoke_ops_total")

// Traces (escape hatch avançado; fluxo padrão é tel.WithSpan)
_, span := c.Trace().Start(parentCtx, "order")
defer span.End()

// Logs (níveis Zap, sem ctx — correlação via contexto-base)
c.Log().Error("boom", "err", err)
c.Log().Info("started")
```

`*Telemetry` já satisfaz `telemetry.Client` (non-breaking). Quando metrics/logging/
tracing estão desligados, os acessores retornam implementações noop (nunca `nil`).

---

## Profiling

O profiling usa somente push para Pyroscope:

1. **Push → Pyroscope** (contínuo): inicia sozinho no `New()` quando há
   `HELLNET_TELEMETRY_ENDPOINT`. O endpoint é **derivado do mesmo endpoint OTLP**:
   - In-cluster (`http://alloy:4318`) → `http://alloy:9999` (porta do `pyroscope.receive_http`)
   - Gateway (`https://alloy.hellnet.com.br`) → `https://alloy.hellnet.com.br/ingest`
   - Override: `HELLNET_TELEMETRY_PROFILE_ENDPOINT` (quando o Alloy não usa a porta 9999)
   Habilita sempre CPU, heap (alloc/inuse), goroutines, **block** e **mutex**.
   Para no `Close()`.

---

## Troubleshooting

| Sintoma | Causa provável | Solução |
|---|---|---|
| Nada aparece no Grafana, mas logs vão para stdout | Endpoint OTLP vazio ou incorreto | Defina `HELLNET_TELEMETRY_ENDPOINT`/`OTEL_EXPORTER_OTLP_ENDPOINT` e valide `/v1/traces`, `/v1/metrics` e `/v1/logs`; a lib não carrega `.env` implicitamente |
| `telemetry iniciado ... Alloy inacessível no startup` | Endpoint não responde (rede/VPN/port-forward) | Valide: `curl -v https://alloy.hellnet.com.br/v1/traces`; use port-forward ou HTTPRoute acessível |
| Traces/Tempo OK, mas metrics não chegam ao collector | Endpoint OTLP ou pipeline de métricas incorreto | Valide o endpoint `/v1/metrics` e a configuração do collector |
| Profiles não no Pyroscope | Endpoint derivado errado (Alloy com porta ≠ 9999) | Sete `HELLNET_TELEMETRY_PROFILE_ENDPOINT` |
| `405` ao testar OTLP com curl GET | Normal — OTLP HTTP usa **POST** | Use `curl -X POST` |

---

## API reference

| Function | Description |
|---|---|
| `telemetry.New()` | Setup all-in-one (sem parâmetros): lê `HELLNET_*` e `OTEL_*` |
| `telemetry.MustNew()` | Como `New`, mas entra em pânico em erro |
| `telemetry.Middleware(tel, handler)` | HTTP tracing + request metrics + logging (request-scoped) |
| `tel.Live()` / `tel.Ready()` / `tel.Health()` | Health probes (`http.Handler`) |
| `tel.HealthRegister(name, fn)` | Custom health check — ctx **fornecido pela lib** |
| `tel.WithSpan(name, fn)` | Span de compatibilidade para jobs sem contexto + erro automático |
| `tel.WithSpanContext(ctx, name, fn)` / `tel.Span(ctx, name, fn)` | Span filho do contexto recebido |
| `tel.Trace().Start(ctx, name)` | Escape hatch avançado: span enraizado num ctx próprio |
| `tel.Meter.Counter/Gauge/Histogram(name)` | Atalhos int64 de métrica |
| `tel.Log().Trace/Debug/Info/Warn/Error/Fatal/Critical(...)` | Logging estruturado (stdout + OTLP) |
| `tel.Worker(job, fn, extra...)` | Job/worker de compatibilidade sem contexto |
| `tel.HTTPClient(opts...)` | `*http.Client` outbound: trace W3C + retry/backoff + métricas `http_client_*` |
| `tel.WatchDB(db, name)` | Métricas automáticas do pool SQL (`db_sql_*`) |
| `tel.Close()` | Flush OTLP |

---

## Tech stack / Architecture

| Pillar | Library | Export |
|---|---|---|
| **Traces** | `go.opentelemetry.io/otel` + `otlptracehttp` | OTLP HTTP → Collector → Tempo |
| **Metrics** | `go.opentelemetry.io/otel` + `otlpmetrichttp` | OTLP HTTP → Collector |
| **Logs** | `go.uber.org/zap` + OTLP bridge | OTLP HTTP → Collector → Loki |

Todos os sinais usam **OTLP HTTP**. gRPC não é suportado na configuração atual.

## Releases

As versões publicadas devem usar tags semver (`vMAJOR.MINOR.PATCH`). Mudanças
de API incompatíveis exigem incremento de major; correções compatíveis usam
minor ou patch conforme o impacto.

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
