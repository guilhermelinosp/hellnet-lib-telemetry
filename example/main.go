package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/guilhermelinosp/hellnet-lib-telemetry/telemetry"
)

func main() {
	// Contexto da aplicação passado UMA vez na construção: tudo que a lib
	// executar (spans, workers, health checks, logs) herda dele.
	ctx := context.Background()

	ops, err := telemetry.New(ctx) // New() carrega .env + valida HELLNET_TELEMETRY_*
	if err != nil {
		log.Fatalf("failed to init telemetry: %v", err)
	}
	defer func() { _ = ops.Shutdown() }()

	mux := http.NewServeMux()
	mux.Handle("GET /live", ops.Live())
	mux.Handle("GET /ready", ops.Ready())
	mux.Handle("GET /health", ops.Health())

	ops.Logger.Info("starting server", "port", 8080)
	if err := http.ListenAndServe(":8080", telemetry.Middleware(ops, mux)); err != nil {
		ops.Logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}
