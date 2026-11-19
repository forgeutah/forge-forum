package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/forgeutah/forge-forum/internal/config"
	"github.com/forgeutah/forge-forum/internal/db"
	"github.com/forgeutah/forge-forum/internal/forum"
	"github.com/forgeutah/forge-forum/internal/move"
	"github.com/forgeutah/forge-forum/internal/notify"
	"github.com/forgeutah/forge-forum/internal/slackx"
	"github.com/forgeutah/forge-forum/internal/web"
)

// interactionsPath is where forge-bot posts the interactions the forum
// owns. The bot's SERVICES setting names it.
const interactionsPath = "/internal/slack/interactions"

// build wires the application together. It returns the HTTP handler, a
// function that starts background work (the outbox worker), and a
// function that stops it.
//
// The forum reaches Slack only through forge-bot (plan KTD5): the handler
// takes the interactions the bot forwards, and Slack API calls go out
// through the bot.
//
// The stop function cancels move jobs in flight, so each leaves its
// mover's failure message in the outbox for the next start to deliver,
// and waits for the jobs and the worker to finish or for its context to
// end, whichever comes first. Call it after the HTTP server has stopped,
// so no more dialogs arrive, and before the database is closed.
// Background work also stops when ctx is done, but then nothing waits
// for it.
func build(ctx context.Context, cfg config.Config, database *db.DB, logger *slog.Logger) (http.Handler, func(), func(context.Context), error) {
	// client stays a nil interface when Slack is not configured; the
	// worker then marks every outbox row skipped (plan KTD5).
	var client slackx.Client
	if cfg.SlackEnabled() {
		// forge-bot stands in for Slack's API: it swaps the forum's service
		// token for the real bot token and passes each call on.
		client = slackx.NewClient(cfg.ForgeBotToken, cfg.ForgeBotURL+"/slack/api/", nil)
	} else {
		logger.Info("Slack is not configured: announcements, reply messages, and Move to forum are off")
	}

	// Background work runs until ctx is done or stop is called.
	ctx, cancel := context.WithCancel(ctx)

	store := forum.NewStore(database, time.Now)
	worker := notify.NewWorker(database, client, logger, time.Now)
	store.SetNotifier(forum.Notifier{
		BaseURL:         cfg.PublicBaseURL,
		AnnounceChannel: cfg.SlackAnnounceChannel,
		Wake:            worker.Wake,
	})

	site, err := web.New(cfg, database, store, logger)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}

	// "Move to forum" needs Slack and a classifier (plan KTD5, KTD8). The
	// same key suggests the dialog's title (KTD13).
	var mover *move.Mover
	switch {
	case cfg.MoveEnabled():
		mover = move.New(move.Deps{
			Slack:      client,
			Store:      store,
			DB:         database,
			Classifier: move.NewClaudeClassifier(cfg.AnthropicAPIKey, cfg.ClassifierModel, "", nil, logger),
			Titles:     move.NewClaudeTitleSuggester(cfg.AnthropicAPIKey, cfg.TitleModel, "", nil, logger),
			BaseURL:    cfg.PublicBaseURL,
			TeamID:     cfg.SlackTeamID,
			Logger:     logger,
			Now:        time.Now,
			Wake:       worker.Wake,
		})
	case cfg.SlackEnabled():
		logger.Info("ANTHROPIC_API_KEY is not set: Move to forum is off")
	}

	mux := http.NewServeMux()
	// Two routes sit outside the proxy contract and its CSRF check: the
	// health check, and the endpoint forge-bot posts interactions to. The
	// bot calls the forum directly over the tailnet, so that endpoint has
	// no member identity; its bearer token is its only gate.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	if mover != nil {
		mux.Handle(interactionsPath, slackx.InteractionsHandler(cfg.ForgeBotToken, cfg.SlackTeamID, mover, logger))
	} else {
		mux.Handle(interactionsPath, http.NotFoundHandler())
	}
	mux.Handle("/", site.Handler())

	var background sync.WaitGroup
	start := func() {
		background.Go(func() {
			worker.Run(ctx)
			logger.Info("outbox worker stopped")
		})
	}
	stop := func(stopCtx context.Context) {
		cancel()
		// End live browser connections: the HTTP server's Shutdown neither
		// closes nor waits for hijacked WebSocket connections.
		site.Close()
		if mover != nil {
			mover.Shutdown()
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			// The HTTP server has stopped, so no more dialogs arrive and
			// no job starts while the jobs are waited for.
			background.Wait()
			if mover != nil {
				mover.Wait()
			}
		}()
		select {
		case <-done:
		case <-stopCtx.Done():
			logger.Warn("background work did not stop in time", "error", stopCtx.Err())
		}
	}
	return mux, start, stop, nil
}
