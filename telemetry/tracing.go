package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/guilhermelinosp/hellnet-lib-telemetry/internal/env"
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

// ContextTracer creates spans using a caller-provided context.
type ContextTracer struct {
	tel *Telemetry
	ctx context.Context
}

// Span runs a function inside a child span.
func (t ContextTracer) Span(name string, fn func(ctx context.Context) error) error {
	_, err := t.tel.runSpan(t.ctx, name, fn)
	return err
}

// buildTracer monta o TracerProvider e o propagador de contexto (sempre registrado globalmente).
func (t *Telemetry) buildTracer(ctx context.Context, o Options, res *sdkresource.Resource) error {
	tp, err := newTracerProvider(ctx, o, res)
	if err != nil {
		return err
	}
	t.tp = tp
	t.tracer = tp.Tracer(o.ServiceName)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return nil
}

// runSpan centraliza o ciclo de vida de um span de aplicação, repassa o ctx
// derivado para fn,
// recupera panics (incrementando exceptions_total, marcando o span como erro e
// re-propagando o panic) e marca erro no span.
func (t *Telemetry) runSpan(parent context.Context, name string, fn func(ctx context.Context) error) (context.Context, error) {
	if parent == nil {
		return nil, errors.New("telemetry: context is required")
	}
	ctx, span := t.rawTrace().Start(parent, name)
	defer func() {
		// Recupera panics automaticamente, contabilizando exceções
		// (exceptions_total) e marcando o span como erro, preservando o
		// comportamento original ao re-propagar o panic.
		if r := recover(); r != nil {
			if t.meter != nil {
				if c, err := t.meter.Int64Counter("exceptions_total"); err == nil {
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
// Use o contexto recebido pelo caller para preservar a linhagem distribuída.
func (t *Telemetry) Trace(ctx context.Context) ContextTracer {
	return ContextTracer{tel: t, ctx: ctx}
}

func (t *Telemetry) rawTrace() trace.Tracer {
	if t.tracer == nil {
		return otel.Tracer(t.serviceName)
	}
	return t.tracer
}

// newTracerProvider cria o TracerProvider SDK. Endpoint vazio → sem export OTLP.
func newTracerProvider(ctx context.Context, opts Options, res *sdkresource.Resource) (*sdktrace.TracerProvider, error) {
	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if sampler := configuredSampler(); sampler != nil {
		tpOpts = append(tpOpts, sdktrace.WithSampler(sampler))
	}

	// Endpoint vazio → sem export OTLP (evita URL "https:" inválida no exporter).
	if opts.OTLPEndpoint != "" {
		exporterOpts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(otlpSignalURL(opts.OTLPEndpoint, "/v1/traces"))}
		if len(opts.OTLPHeaders) > 0 {
			exporterOpts = append(exporterOpts, otlptracehttp.WithHeaders(opts.OTLPHeaders))
		}
		exporter, err := otlptracehttp.New(ctx, exporterOpts...)
		if err != nil {
			return nil, err
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exporter))
	}

	return sdktrace.NewTracerProvider(tpOpts...), nil
}

// configuredSampler honors the standard OTel sampler environment variables.
// An invalid value returns nil so the SDK keeps its safe default.
func configuredSampler() sdktrace.Sampler {
	switch strings.ToLower(strings.TrimSpace(env.String("OTEL_TRACES_SAMPLER", ""))) {
	case "always_on":
		return sdktrace.AlwaysSample()
	case "always_off":
		return sdktrace.NeverSample()
	case "traceidratio":
		ratio, err := strconv.ParseFloat(env.String("OTEL_TRACES_SAMPLER_ARG", ""), 64)
		if err == nil && ratio >= 0 && ratio <= 1 {
			return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
		}
	}
	return nil
}
