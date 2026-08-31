package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Tracer abstrai a criação de spans (assinatura idêntica a trace.Tracer.Start).
//
// ESCAPE HATCH avançado: recebe ctx do CALLER explicitamente e não faz parte
// do fluxo padrão de correlação (que é raiz(baseCtx) → filhos via
// WithSpan/Worker). A superfície ctx-free é WithSpan/Worker apenas.
type Tracer interface {
	Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span)
}

// spanKey marca internamente ctxs carregando um span criado por ESTA lib
// (runBaseSpan). Permite distinguir spans app-level (WithSpan/Worker) de spans
// request-scoped extraídos pelo Middleware (estes NÃO carregam a chave), e é o
// mecanismo pelo qual chamadas aninhadas enxergam o pai ativo.
type spanKey struct{}

// spanEntry guarda um spanCtx ativo na pilha de aninhamento da instância. É
// ponteiro para permitir remoção por identidade sem comparar ctxs (==) diretamente.
type spanEntry struct {
	ctx context.Context
}

// buildTracer monta o TracerProvider e o propagador de contexto (sempre registrado globalmente).
func (t *Telemetry) buildTracer(o Options, res *sdkresource.Resource) error {
	tp, err := newTracerProvider(o, res)
	if err != nil {
		return err
	}
	t.tp = tp
	t.Tracer = tp.Tracer(o.ServiceName)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return nil
}

// WithSpan cria um span, executa fn e finaliza. Em erro, marca o span com o
// status. O ctx derivado (contendo o span) é repassado para fn, permitindo que
// código otel-instrumentado mais a fundo continue a linhagem.
//
// Aninhamento automático: chamadas a WithSpan/Worker DENTRO de fn tornam-se
// FILHAS do span ativo desta lib (a lib guarda internamente o spanCtx atual;
// o pai preferido é esse ctx quando presente, caindo para o baseCtx na raiz).
// Assim a linhagem aninha naturalmente: raiz(baseCtx) → outer → inner → …,
// sem precisar repassar ctx pela API pública (que permanece ctx-free).
func (t *Telemetry) WithSpan(name string, fn func(ctx context.Context) error) error {
	_, err := t.runBaseSpan(name, fn)
	return err
}

// currentSpanParent devolve o pai do próximo span criado por runBaseSpan:
// quando há um span desta lib ativo (topo da pilha de aninhamento), retorna-o
// (o novo span nasce FILHO dele); caso contrário (ou se o span do topo não é
// mais válido — ex.: tracing desligado), enraíza no contexto-base.
func (t *Telemetry) currentSpanParent() context.Context {
	t.spanMu.Lock()
	defer t.spanMu.Unlock()
	if n := len(t.spanStack); n > 0 {
		if sc := trace.SpanContextFromContext(t.spanStack[n-1].ctx); sc.IsValid() {
			return t.spanStack[n-1].ctx
		}
	}
	return t.baseCtx
}

// pushSpanCtx empilha o spanCtx recém-criado (chamado em runBaseSpan).
func (t *Telemetry) pushSpanCtx(e *spanEntry) {
	t.spanMu.Lock()
	defer t.spanMu.Unlock()
	t.spanStack = append(t.spanStack, e)
}

// popSpanCtx remove a própria entrada da pilha de aninhamento (LIFO; o defer
// garante a execução mesmo quando um panic é re-propagado).
func (t *Telemetry) popSpanCtx(e *spanEntry) {
	t.spanMu.Lock()
	defer t.spanMu.Unlock()
	for i := len(t.spanStack) - 1; i >= 0; i-- {
		if t.spanStack[i] == e {
			t.spanStack = append(t.spanStack[:i], t.spanStack[i+1:]...)
			return
		}
	}
}

// runBaseSpan centraliza o ciclo de vida de um span de aplicação para
// WithSpan/Worker: deriva o ctx do pai ativo (span aninhado desta lib quando
// presente, senão o contexto-base raiz), repassa o ctx derivado para fn,
// recupera panics (incrementando exceptions_total, marcando o span como erro e
// re-propagando o panic) e marca erro no span. Retorna o ctx contendo o span
// (útil p/ correlação de métricas/log internos).
func (t *Telemetry) runBaseSpan(name string, fn func(ctx context.Context) error) (context.Context, error) {
	parent := t.currentSpanParent()
	rawCtx, span := t.Trace().Start(parent, name)
	// Marca o ctx como portador de span desta lib (spanKey): chamadas aninhadas
	// de WithSpan/Worker reconhecem-no como pai automático.
	entry := &spanEntry{}
	entry.ctx = context.WithValue(rawCtx, spanKey{}, struct{}{})
	ctx := entry.ctx
	t.pushSpanCtx(entry)
	defer t.popSpanCtx(entry)
	defer func() {
		// Recupera panics automaticamente, contabilizando exceções
		// (exceptions_total) e marcando o span como erro, preservando o
		// comportamento original ao re-propagar o panic.
		if r := recover(); r != nil {
			if t.Meter != nil {
				if c, err := t.Meter.Counter("exceptions_total"); err == nil {
					c.Add(ctx, 1, metric.WithAttributes(attribute.String("span", name), attribute.String("kind", "panic")))
				}
			}
			span.RecordError(fmt.Errorf("%v", r))
			span.SetStatus(codes.Error, fmt.Sprintf("%v", r))
			panic(r)
		}
	}()
	defer span.End()
	if err := fn(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return ctx, err
	}
	return ctx, nil
}

// Trace retorna a abstração de traces. Nome evita colisão com o campo exportado Tracer.
//
// Uso interno da lib SEMPRE deriva do contexto-base (runBaseSpan); use apenas
// como escape hatch avançado quando precisar enraizar spans num ctx próprio.
func (t *Telemetry) Trace() Tracer {
	if t.Tracer == nil {
		return otel.Tracer(t.serviceName)
	}
	return t.Tracer
}

// newTracerProvider cria o TracerProvider SDK. Endpoint vazio → sem export OTLP.
func newTracerProvider(opts Options, res *sdkresource.Resource) (*sdktrace.TracerProvider, error) {
	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}

	// Endpoint vazio → sem export OTLP (evita URL "https:" inválida no exporter).
	if opts.OTLPEndpoint != "" {
		exporter, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(otlpSignalURL(opts.OTLPEndpoint, "/v1/traces")))
		if err != nil {
			return nil, err
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exporter))
	}

	return sdktrace.NewTracerProvider(tpOpts...), nil
}
