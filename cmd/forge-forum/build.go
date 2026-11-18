package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/notify"
	"github.com/forgeutah/forge-forum/internal/slackx"
	"github.com/forgeutah/forge-forum/internal/web"
)

// build wires the application together. It returns the HTTP handler and a
// function that starts background work (Slack connection, outbox worker).
func build(ctx context.Context, cfg config.Config, database *db.DB, logger *slog.Logger) (http.Handler, func(), error) {
	// client stays a nil interface when Slack is not configured; the
	// worker then marks every outbox row skipped (plan KTD5).
	var client slackx.Client
	if cfg.SlackEnabled() {
		client = slackx.NewClient(cfg.SlackBotToken, "", nil)
	} else {
		logger.Info("Slack is not configured: announcements, reply messages, and Move to forum are off")
	}

	store := forum.NewStore(database, time.Now)
	worker := notify.NewWorker(database, client, logger, time.Now)
	store.SetNotifier(forum.Notifier{
		BaseURL:         cfg.PublicBaseURL,
		AnnounceChannel: cfg.SlackAnnounceChannel,
		Wake:            worker.Wake,
	})

	site, err := web.New(cfg, database, store, logger)
	if err != nil {
		return nil, nil, err
	}

	mux := http.NewServeMux()
	// The health check is the only route outside the proxy contract.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.Handle("/", site.Handler())

	start := func() {
		go worker.Run(ctx)

		// MOVE-TO-FORUM HOOKUP: start the Socket Mode loop here, when
		// cfg.MoveEnabled(). In scope: ctx, cfg, logger, database, client
		// (nil unless cfg.SlackEnabled()), store, and worker (call
		// worker.Wake after a move enqueues its link-back).
	}
	return mux, start, nil
}
