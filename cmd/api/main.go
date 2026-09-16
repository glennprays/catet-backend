package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Embed tzdata so Asia/Jakarta day boundaries resolve in any
	// container image.
	_ "time/tzdata"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/auth"
	"github.com/glennprays/catet-backend/internal/infrastructure"
	"github.com/glennprays/catet-backend/internal/middleware"
	"github.com/glennprays/log"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

func main() {
	lifecycleID := uuid.New().String()
	// Initialize app dependencies via Wire
	app, err := infrastructure.InitializeApp()
	if err != nil {
		panic(fmt.Sprintf("Failed to initialize app: %v", err))
	}
	defer func() {
		_ = app.Logger.Sync()
		app.Pool.Close()
	}()
	logger := app.Logger.With(log.String("component", "main"))

	// The first boot of a fresh or migrated install turns the
	// placeholder admin into a real account. Fatal on failure: an
	// unclaimed admin means nobody can ever log in, and booting anyway
	// would hide that.
	claimCtx, claimCancel := context.WithTimeout(context.Background(), 15*time.Second)
	claimErr := auth.ClaimBootstrapAdmin(claimCtx, app.Users, app.Gateway, app.Config, logger)
	claimCancel()
	if claimErr != nil {
		logger.Fatal(lifecycleID, "bootstrap admin claim failed", map[string]any{"error": claimErr.Error()})
	}

	// Create Fiber app with custom error handler and explicit limits.
	// Defaults (15s/15s read/write, 4 MiB body) are not appropriate for
	// production — slow clients can tie up connections and large bodies
	// reach handlers without backpressure.
	fiberApp := fiber.New(fiber.Config{
		AppName:               app.Config.AppName,
		ErrorHandler:          middleware.ErrorHandler(app.Logger),
		DisableStartupMessage: true,
		ReadTimeout:           10 * time.Second,
		WriteTimeout:          10 * time.Second,
		IdleTimeout:           120 * time.Second,
		BodyLimit:             1 << 20, // 1 MiB
		// The Go API is internal-only and always sits behind the Next.js
		// BFF, so the forwarded client address is the only real one — and
		// the auth throttles are keyed by it.
		ProxyHeader: fiber.HeaderXForwardedFor,
	})

	// Setup routes (includes global middleware in correct order)
	app.Router.Setup(fiberApp)

	// Start server in goroutine
	addr := fmt.Sprintf(":%d", app.Config.AppPort)
	go func() {
		logger.Info(lifecycleID, "Starting server", map[string]any{
			"address":  addr,
			"app_name": app.Config.AppName,
			"pid":      os.Getpid(),
		})
		if err := fiberApp.Listen(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal(lifecycleID, "Failed to start server", map[string]any{
				"error": err.Error(),
			})
		}
	}()

	// Bot boot: register the webhook subscription and replay anything a
	// crash left mid-flight. Never fatal — the API serves the dashboard
	// with or without a reachable gateway.
	botCtx, botCancel := context.WithTimeout(context.Background(), 30*time.Second)
	app.Bot.Start(botCtx)
	botCancel()

	// The bill-reminder ticker: the one scheduled outbound path (ADR-0006).
	// A minute of granularity is enough for an hourly send window and
	// keeps the loop cheap.
	remCtx, remCancel := context.WithCancel(context.Background())
	if app.Config.BillRemindersEnabled {
		go app.Reminders.Run(remCtx, time.Minute)
	}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	remCancel()
	logger.Info(lifecycleID, "Shutting down server", nil)

	// Timeout context for shutdown
	timeoutSeconds := 10
	if app.Config.Env == config.DEV {
		timeoutSeconds = 0 // No timeout in dev for easier debugging
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	if err := fiberApp.ShutdownWithContext(ctx); err != nil {
		logger.Fatal(lifecycleID, "Server forced to shutdown", map[string]any{
			"error": err.Error(),
		})
	}

	logger.Info(lifecycleID, "Server exited", nil)
}
