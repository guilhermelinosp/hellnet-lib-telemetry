# Changelog

## Unreleased

- Adicionado `instrument.WithoutTracing(ctx)`: spans criados sob o contexto retornado
  não são gravados (pai não amostrado), para polling em background.

### Breaking

- Removido o pacote `telemetrytest`. Migração: use `telemetry.NewHarness(t)` do
  pacote `telemetry` (mesmos métodos; `New` passa a `NewHarness`).

## v1.9.1 - 2026-09-29

- Corrigido o filtro de `LogLevel` no contrato `instrument.Logger` para stdout
  e OTLP.
- Corrigida a mensagem do logger contratual para usar o texto sanitizado no
  campo `msg` do stdout.
- Restaurados providers SDK sem exporter quando o endpoint OTLP está vazio,
  mantendo spans locais correlacionáveis sem exportação.
- Corrigida a precedência de headers minúsculos no carrier Kafka.
- O pacote `telemetrytest` permanece disponível como compatibilidade
  deprecated da v1.9.0, mas não é usado pelos testes internos.

## v1.9.0

### Breaking

- O campo público `Telemetry.Logger` foi substituído pelo método
  `Telemetry.Logger(scope string)` para implementar `instrument.Instrumentation`.
  Migração: `tel.Logger.Infow("msg", fields...)` passa a ser
  `tel.Log(ctx).Info("msg", fields...)`.

- Adicionados os contratos leves `instrument` e `messaging`, sem dependência do
  SDK OpenTelemetry, Zap, `otelhttp` ou exporters.
- `*telemetry.Telemetry` agora implementa diretamente `instrument.Instrumentation`.
- Migração de `telemetry.Client`: libs novas devem receber
  `instrument.Instrumentation`; a API `Client.Trace(context.Context)` foi
  preservada. O campo legado `Telemetry.Logger` foi substituído pelo método
  escopado `Telemetry.Logger(scope)`; o caminho legado continua disponível por
  `Log(ctx)`.
