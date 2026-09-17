// Hand-maintained injector mirroring internal/infrastructure/wire.go.
//
// wire v0.7.0's type loader cannot read Go 1.25 export data
// ("package X without types"), so `make generate` cannot run on this
// toolchain. This file carries the same content `wire gen` would
// emit; re-run `make generate` and reconcile once a wire release
// supports the toolchain.

//go:build !wireinject
// +build !wireinject

package infrastructure

import (
	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/auth"
	"github.com/glennprays/catet-backend/internal/bot"
	"github.com/glennprays/catet-backend/internal/gateway"
	"github.com/glennprays/catet-backend/internal/handler"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/router"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/pkg/logger"
)

// InitializeApp wires the whole application graph.
func InitializeApp() (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	logger, err := logger.ProviderLogger(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := ProvidePool(cfg)
	if err != nil {
		return nil, err
	}

	// Repositories.
	sourceRepo := repository.NewSourceRepository(pool)
	transactionRepo := repository.NewTransactionRepository(pool)
	categoryRepo := repository.NewCategoryRepository(pool)
	merchantRepo := repository.NewMerchantRepository(pool)
	billRepo := repository.NewBillRepository(pool)
	billPaymentRepo := repository.NewBillPaymentRepository(pool)
	reminderSendRepo := repository.NewBillReminderSendRepository(pool)
	budgetRepo := repository.NewBudgetRepository(pool)
	settingsRepo := repository.NewSettingsRepository(pool)
	pendingRepo := repository.NewPendingActionRepository(pool)
	analyticsRepo := repository.NewAnalyticsRepository(pool)
	llmProviderRepo := repository.NewLLMProviderRepository(pool)

	inboundRepo := repository.NewInboundMessageRepository(pool)
	chatStateRepo := repository.NewBotChatStateRepository(pool)
	userRepo := repository.NewUserRepository(pool)
	inviteRepo := repository.NewInviteRepository(pool)
	resetRepo := repository.NewPasswordResetRepository(pool)
	otpRepo := repository.NewOtpRepository(pool)

	// Services. The LLM client is built first: merchant canonicalisation
	// is a write-path dependency, so the ledger service needs it before
	// anything can be captured.
	llmProviderSvc := service.NewLLMProviderService(llmProviderRepo, cfg, provideSecretBox(cfg), logger)
	llmClient := provideLLMClient(llmProviderSvc)
	merchantSvc := service.NewMerchantService(merchantRepo, llmClient, logger)
	transactionSvc := service.NewTransactionService(transactionRepo, sourceRepo, categoryRepo, merchantSvc, billPaymentRepo, billRepo, logger)
	sourceSvc := service.NewSourceService(sourceRepo, transactionSvc)
	categorySvc := service.NewCategoryService(categoryRepo)
	billSvc := service.NewBillService(billRepo, billPaymentRepo, transactionSvc, sourceRepo)
	analyticsSvc := service.NewAnalyticsService(analyticsRepo, categoryRepo, sourceRepo, billRepo, settingsRepo)
	budgetSvc := service.NewBudgetService(budgetRepo, categoryRepo, analyticsSvc)
	overviewSvc := service.NewOverviewService(sourceRepo, billRepo, settingsRepo, analyticsRepo, pendingRepo, transactionSvc)
	badgesSvc := service.NewBadgesService(analyticsRepo, pendingRepo, billRepo)
	settingsSvc := service.NewSettingsService(settingsRepo, sourceRepo)
	onboardingSvc := service.NewOnboardingService(categoryRepo, sourceRepo)

	// Auth. The invite service is built before the bot: the harness
	// re-sends an invited stranger's register link through it.
	waSender := gateway.Provide(cfg, logger)
	tokenSvc := auth.ProvideTokenService(cfg)
	otpSender := auth.ProvideOtpSender(waSender)
	otpSvc := auth.ProvideOtpService(otpRepo, otpSender, cfg)
	authSvc := auth.ProvideAuthService(userRepo, inviteRepo, resetRepo, otpSvc, tokenSvc, onboardingSvc, waSender)
	inviteSvc := auth.ProvideInviteService(inviteRepo, userRepo, waSender, logger, cfg)
	authHandler := auth.NewHandler(authSvc)
	authMiddleware := auth.ProvideRequireAuth(tokenSvc, userRepo)

	// WhatsApp bot. The harness is also the Hold Queue's executor, so a
	// draft confirmed from the dashboard runs the same write path as one
	// confirmed in chat.
	botHarness := bot.NewHarness(
		cfg, logger, waSender, llmClient,
		transactionSvc, merchantSvc, overviewSvc, analyticsSvc, billSvc,
		sourceRepo, categoryRepo, transactionRepo, pendingRepo,
		inboundRepo, chatStateRepo, analyticsRepo, settingsRepo,
		userRepo, billRepo, reminderSendRepo, inviteSvc,
	)
	holdQueueSvc := service.NewHoldQueueService(pendingRepo, botHarness)

	// Handlers.
	sourceHandler := handler.NewSourceHandler(sourceSvc)
	transactionHandler := handler.NewTransactionHandler(transactionSvc)
	categoryHandler := handler.NewCategoryHandler(categorySvc)
	merchantHandler := handler.NewMerchantHandler(merchantSvc)
	billHandler := handler.NewBillHandler(billSvc)
	budgetHandler := handler.NewBudgetHandler(budgetSvc)
	overviewHandler := handler.NewOverviewHandler(overviewSvc, badgesSvc)
	analyticsHandler := handler.NewAnalyticsHandler(analyticsSvc)
	holdQueueHandler := handler.NewHoldQueueHandler(holdQueueSvc)
	settingsHandler := handler.NewSettingsHandler(settingsSvc)
	userHandler := handler.NewUserHandler(userRepo, inviteSvc)
	adminLLMHandler := handler.NewAdminLLMHandler(llmProviderSvc)
	healthHandler := handler.NewHealthHandler()
	versionHandler := handler.NewVersionHandler()
	// Verification needs the shared secret; without one the route is not
	// mounted, so the handler is not built either.
	var webhookHandler *handler.WebhookHandler
	if cfg.WaWebhookSecret != "" {
		webhookHandler = handler.NewWebhookHandler(provideWebhookVerifier(cfg), botHarness)
	}

	routerRouter := router.NewRouter(
		cfg,
		logger,
		healthHandler,
		versionHandler,
		authHandler,
		authMiddleware,
		sourceHandler,
		transactionHandler,
		categoryHandler,
		merchantHandler,
		billHandler,
		budgetHandler,
		overviewHandler,
		analyticsHandler,
		holdQueueHandler,
		settingsHandler,
		userHandler,
		adminLLMHandler,
		webhookHandler,
	)

	reminderSvc := service.NewReminderService(userRepo, billRepo, reminderSendRepo, settingsRepo, waSender, pool, logger)

	app := &App{
		Config:  cfg,
		Logger:  logger,
		Pool:    pool,
		Router:  routerRouter,
		Bot:     botHarness,
		Users:   userRepo,
		Gateway: waSender,

		Reminders: reminderSvc,
	}
	return app, nil
}
