package router

import (
	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/auth"
	"github.com/glennprays/catet-backend/internal/handler"
	"github.com/glennprays/catet-backend/internal/middleware"
	"github.com/glennprays/log"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/expvar"
	"github.com/gofiber/fiber/v2/middleware/pprof"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

type Router struct {
	logger *log.Logger
	config *config.Config

	HealthHandler  *handler.HealthHandler
	VersionHandler *handler.VersionHandler
	AuthHandler    *auth.Handler

	// RequireAuth guards every route below the public set.
	AuthMiddleware fiber.Handler

	SourceHandler      *handler.SourceHandler
	TransactionHandler *handler.TransactionHandler
	CategoryHandler    *handler.CategoryHandler
	MerchantHandler    *handler.MerchantHandler
	BillHandler        *handler.BillHandler
	BudgetHandler      *handler.BudgetHandler
	OverviewHandler    *handler.OverviewHandler
	AnalyticsHandler   *handler.AnalyticsHandler
	HoldQueueHandler   *handler.HoldQueueHandler
	SettingsHandler    *handler.SettingsHandler
	UserHandler        *handler.UserHandler
	AdminLLMHandler    *handler.AdminLLMHandler
	// WebhookHandler is nil when the bot is not configured.
	WebhookHandler *handler.WebhookHandler
}

func NewRouter(
	cfg *config.Config,
	logger *log.Logger,
	healthHandler *handler.HealthHandler,
	versionHandler *handler.VersionHandler,
	authHandler *auth.Handler,
	authMiddleware fiber.Handler,
	sourceHandler *handler.SourceHandler,
	transactionHandler *handler.TransactionHandler,
	categoryHandler *handler.CategoryHandler,
	merchantHandler *handler.MerchantHandler,
	billHandler *handler.BillHandler,
	budgetHandler *handler.BudgetHandler,
	overviewHandler *handler.OverviewHandler,
	analyticsHandler *handler.AnalyticsHandler,
	holdQueueHandler *handler.HoldQueueHandler,
	settingsHandler *handler.SettingsHandler,
	userHandler *handler.UserHandler,
	adminLLMHandler *handler.AdminLLMHandler,
	webhookHandler *handler.WebhookHandler,
) *Router {
	routerLogger := logger.With(log.String("component", "router"))
	return &Router{
		logger:             routerLogger,
		config:             cfg,
		HealthHandler:      healthHandler,
		VersionHandler:     versionHandler,
		AuthHandler:        authHandler,
		AuthMiddleware:     authMiddleware,
		SourceHandler:      sourceHandler,
		TransactionHandler: transactionHandler,
		CategoryHandler:    categoryHandler,
		MerchantHandler:    merchantHandler,
		BillHandler:        billHandler,
		BudgetHandler:      budgetHandler,
		OverviewHandler:    overviewHandler,
		AnalyticsHandler:   analyticsHandler,
		HoldQueueHandler:   holdQueueHandler,
		SettingsHandler:    settingsHandler,
		UserHandler:        userHandler,
		AdminLLMHandler:    adminLLMHandler,
		WebhookHandler:     webhookHandler,
	}
}

// Setup configures all application routes. Public set: /health,
// /version, /auth/otp/*. Everything else requires a bearer session.
func (r *Router) Setup(app *fiber.App) {
	// Global middleware. TraceID must run before recover so that any
	// panic surfaced by the recovery middleware can be correlated by
	// trace_id in the logs.
	app.Use(middleware.TraceID())
	app.Use(recover.New())
	app.Use(middleware.CORS(r.config.CorsAllowedOrigins))

	app.Use(middleware.NewHTTPLogger(r.logger))

	// Dev-only profiling. Mounted before the v1 group so /debug/pprof
	// is at the root. Stripped in production so the profiler isn't
	// exposed to the internet.
	if r.config.Env != config.PROD {
		app.Use(pprof.New())
		// Counters (bot_dropped_total, …) ride the same dev-only surface:
		// useful while pairing the bot, never exposed in production.
		app.Use(expvar.New())
	}

	// API v1 group
	v1 := app.Group("/api/v1")

	// Public routes (no auth). Everything the unauthenticated dashboard
	// needs: logging in, asking for a code, reading an invite, signing
	// up, and resetting a forgotten password.
	r.setupHealthRoutes(v1)
	r.setupVersionRoutes(v1)
	r.setupPublicAuthRoutes(v1.Group("/auth"))

	// The gateway webhook is the one route the internet reaches. It is
	// authenticated by the subscription's HMAC signature, never by a
	// session, so it must stay outside the protected group. Without a
	// secret there is nothing to verify against and the route is not
	// mounted at all.
	if r.WebhookHandler != nil && r.config.WaWebhookSecret != "" {
		v1.Post("/webhooks/whatsapp", r.WebhookHandler.Incoming)
	} else {
		r.logger.Warn("", "webhook route disabled: WA_WEBHOOK_SECRET empty", nil)
	}

	// Protected routes.
	protected := v1.Group("", r.AuthMiddleware)
	r.setupAuthRoutes(protected)
	r.setupSourceRoutes(protected)
	r.setupTransactionRoutes(protected)
	r.setupCategoryRoutes(protected)
	r.setupMerchantRoutes(protected)
	r.setupBillRoutes(protected)
	r.setupBudgetRoutes(protected)
	r.setupOverviewRoutes(protected)
	r.setupAnalyticsRoutes(protected)
	r.setupHoldQueueRoutes(protected)
	r.setupSettingsRoutes(protected)
	// Account management is admin-only; the flag is checked once, at the
	// group.
	r.setupUserRoutes(protected.Group("/users", auth.RequireAdmin))
	// The AI provider is instance-wide configuration, so it sits behind
	// the same admin gate.
	r.setupAdminRoutes(protected.Group("/admin", auth.RequireAdmin))
}

func (r *Router) setupHealthRoutes(group fiber.Router) {
	group.Get("/health", r.HealthHandler.Check)
}

func (r *Router) setupVersionRoutes(group fiber.Router) {
	group.Get("/version", r.VersionHandler.Get)
}

// setupPublicAuthRoutes mounts the unauthenticated half of /auth.
func (r *Router) setupPublicAuthRoutes(group fiber.Router) {
	group.Post("/login", r.AuthHandler.Login)
	group.Post("/otp/request", r.AuthHandler.RequestOtp)
	group.Get("/invites/:token", r.AuthHandler.InviteInfo)
	group.Post("/register", r.AuthHandler.Register)
	group.Post("/password/reset/verify", r.AuthHandler.VerifyReset)
	group.Post("/password/reset/complete", r.AuthHandler.CompleteReset)
}

func (r *Router) setupAuthRoutes(group fiber.Router) {
	group.Get("/auth/me", r.AuthHandler.Me)
	group.Post("/auth/password/change", r.AuthHandler.ChangePassword)
	group.Post("/auth/logout-all", r.AuthHandler.LogoutAll)
}

func (r *Router) setupUserRoutes(group fiber.Router) {
	group.Get("", r.UserHandler.List)
	group.Post("/invites", r.UserHandler.CreateInvite)
	group.Get("/invites", r.UserHandler.ListInvites)
	group.Delete("/invites/:id", r.UserHandler.RevokeInvite)
	group.Post("/:id/deactivate", r.UserHandler.Deactivate)
	group.Post("/:id/activate", r.UserHandler.Activate)
}

// setupAdminRoutes mounts instance-wide configuration. /llm is the
// OpenAI-compatible provider the bot uses; saving it takes effect on
// the next request, without a restart.
func (r *Router) setupAdminRoutes(group fiber.Router) {
	group.Get("/llm", r.AdminLLMHandler.Get)
	group.Put("/llm", r.AdminLLMHandler.Update)
	group.Delete("/llm", r.AdminLLMHandler.Reset)
	group.Post("/llm/models", r.AdminLLMHandler.Models)
	group.Post("/llm/test", r.AdminLLMHandler.Test)
}

func (r *Router) setupSourceRoutes(group fiber.Router) {
	group.Get("/sources", r.SourceHandler.List)
	group.Post("/sources", r.SourceHandler.Create)
	group.Patch("/sources/:id", r.SourceHandler.Update)
	group.Delete("/sources/:id", r.SourceHandler.Archive)
	// Registered explicitly so "permanent" is never read as an id.
	group.Delete("/sources/:id/permanent", r.SourceHandler.DeletePermanently)
	group.Post("/sources/:id/restore", r.SourceHandler.Restore)
	group.Post("/sources/:id/adjustments", r.SourceHandler.Adjust)
}

func (r *Router) setupTransactionRoutes(group fiber.Router) {
	group.Get("/transactions", r.TransactionHandler.List)
	group.Post("/transactions", r.TransactionHandler.Create)
	group.Get("/transactions/:id", r.TransactionHandler.Get)
	group.Patch("/transactions/:id", r.TransactionHandler.Update)
	group.Delete("/transactions/:id", r.TransactionHandler.Void)
	group.Post("/transactions/:id/reversals", r.TransactionHandler.Reverse)
}

func (r *Router) setupMerchantRoutes(group fiber.Router) {
	group.Get("/merchants", r.MerchantHandler.List)
	group.Patch("/merchants/:id", r.MerchantHandler.Rename)
	group.Post("/merchants/:id/merge", r.MerchantHandler.Merge)
}

func (r *Router) setupCategoryRoutes(group fiber.Router) {
	group.Get("/categories", r.CategoryHandler.List)
	group.Post("/categories", r.CategoryHandler.Create)
	group.Patch("/categories/:id", r.CategoryHandler.Update)
	group.Delete("/categories/:id", r.CategoryHandler.Archive)
}

func (r *Router) setupBillRoutes(group fiber.Router) {
	group.Get("/bills", r.BillHandler.List)
	group.Post("/bills", r.BillHandler.Create)
	// Registered before the /bills/:id routes so "payments" is not
	// captured as a bill id.
	group.Get("/bills/payments", r.BillHandler.Payments)
	group.Patch("/bills/:id", r.BillHandler.Update)
	group.Delete("/bills/:id", r.BillHandler.Deactivate)
	group.Post("/bills/:id/activate", r.BillHandler.Reactivate)
	group.Post("/bills/:id/payments", r.BillHandler.Pay)
}

func (r *Router) setupBudgetRoutes(group fiber.Router) {
	group.Get("/budgets", r.BudgetHandler.List)
	group.Post("/budgets", r.BudgetHandler.Create)
	group.Patch("/budgets/:id", r.BudgetHandler.Update)
	group.Delete("/budgets/:id", r.BudgetHandler.Delete)
}

func (r *Router) setupOverviewRoutes(group fiber.Router) {
	group.Get("/overview", r.OverviewHandler.Get)
	group.Get("/badges", r.OverviewHandler.Badges)
}

func (r *Router) setupAnalyticsRoutes(group fiber.Router) {
	analytics := group.Group("/analytics")
	analytics.Get("/category-breakdown", r.AnalyticsHandler.CategoryBreakdown)
	analytics.Get("/income-expense", r.AnalyticsHandler.IncomeExpense)
	analytics.Get("/sts-trend", r.AnalyticsHandler.StsTrend)
	analytics.Get("/heatmap", r.AnalyticsHandler.Heatmap)
	analytics.Get("/top-merchants", r.AnalyticsHandler.TopMerchants)
	analytics.Get("/essential-lifestyle", r.AnalyticsHandler.EssentialLifestyle)
}

func (r *Router) setupHoldQueueRoutes(group fiber.Router) {
	group.Get("/hold-queue", r.HoldQueueHandler.List)
	group.Post("/hold-queue/:id/resolve", r.HoldQueueHandler.Resolve)
}

func (r *Router) setupSettingsRoutes(group fiber.Router) {
	group.Get("/settings", r.SettingsHandler.Get)
	group.Put("/settings", r.SettingsHandler.Update)
}
