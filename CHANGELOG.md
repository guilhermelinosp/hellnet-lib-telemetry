# Changelog

## v1.9.1 - 2026-09-29

- Corrigido o filtro de `LogLevel` no contrato `instrument.Logger` para stdout
  e OTLP.
- Corrigida a mensagem do logger contratual para usar o texto sanitizado no
  campo `msg` do stdout.
- Restaurados providers SDK sem exporter quando o endpoint OTLP está vazio,
  mantendo spans locais correlacionáveis sem exportação.
- `telemetrytest.Reset()` mantém instrumentos e o `MeterProvider`; métricas do
  harness são cumulativas entre resets.
- Adicionados helpers de métricas, spans e logs ao `telemetrytest`.
- Corrigida a precedência de headers minúsculos no carrier Kafka.

## v1.9.0

### Breaking

- O campo público `Telemetry.Logger` foi substituído pelo método
  `Telemetry.Logger(scope string)` para implementar `instrument.Instrumentation`.
  Migração: `tel.Logger.Infow("msg", fields...)` passa a ser
  `tel.Log(ctx).Info("msg", fields...)`.

- Adicionados os contratos leves `instrument` e `messaging`, sem dependência do
  SDK OpenTelemetry, Zap, `otelhttp` ou exporters.
- Adicionado `telemetrytest` para testes de spans, métricas e logs em memória.
- `*telemetry.Telemetry` agora implementa diretamente `instrument.Instrumentation`.
- Migração de `telemetry.Client`: libs novas devem receber
  `instrument.Instrumentation`; a API `Client.Trace(context.Context)` foi
  preservada. O campo legado `Telemetry.Logger` foi substituído pelo método
  escopado `Telemetry.Logger(scope)`; o caminho legado continua disponível por
  `Log(ctx)`.
