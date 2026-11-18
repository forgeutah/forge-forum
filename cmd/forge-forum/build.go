package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/db"
)

// build wires the application together. It returns the HTTP handler and a
// function that starts background work (Slack connection, outbox worker).
func build(ctx context.Context, cfg config.Config, database *db.DB, logger *slog.Logger) (http.Handler, func(), error) {
	mux := http.NewServeMux()
	// The health check is the only route outside the proxy contract.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	return mux, func() {}, nil
}
