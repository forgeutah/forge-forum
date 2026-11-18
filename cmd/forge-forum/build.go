package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/move"
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
	var realClient *slackx.RealClient
	if cfg.SlackEnabled() {
		realClient = slackx.NewClient(cfg.SlackBotToken, "", nil)
		client = realClient
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

	// "Move to forum" needs Slack and a classifier (plan KTD5, KTD8).
	var mover *move.Mover
	switch {
	case cfg.MoveEnabled():
		mover = move.New(move.Deps{
			Slack:      client,
			Store:      store,
			DB:         database,
			Classifier: move.NewClaudeClassifier(cfg.AnthropicAPIKey, cfg.ClassifierModel, "", nil),
			BaseURL:    cfg.PublicBaseURL,
			TeamID:     cfg.SlackTeamID,
			Logger:     logger,
			Now:        time.Now,
			Wake:       worker.Wake,
		})
	case cfg.SlackEnabled():
		logger.Info("ANTHROPIC_API_KEY is not set: Move to forum is off")
	}

	start := func() {
		go worker.Run(ctx)
		if mover == nil {
			return
		}
		go func() {
			err := slackx.RunSocketMode(ctx, realClient, cfg.SlackAppToken, mover, logger)
			if err != nil && ctx.Err() == nil {
				logger.Error("Slack Socket Mode stopped; Move to forum is off until restart", "error", err)
			}
		}()
	}
	return mux, start, nil
}
