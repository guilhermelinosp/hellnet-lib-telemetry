# Changelog

## Unreleased

- Adicionados os contratos leves `instrument` e `messaging`, sem dependência do
  SDK OpenTelemetry, Zap, `otelhttp` ou exporters.
- Adicionado `telemetrytest` para testes de spans, métricas e logs em memória.
- `*telemetry.Telemetry` agora implementa diretamente `instrument.Instrumentation`.
- Migração de `telemetry.Client`: libs novas devem receber
  `instrument.Instrumentation`; a API `Client.Trace(context.Context)` foi
  preservada. O campo legado `Telemetry.Logger` foi substituído pelo método
  escopado `Telemetry.Logger(scope)`; o caminho legado continua disponível por
  `Log(ctx)`.
