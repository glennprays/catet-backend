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
	billRepo := repository.NewBillRepository(pool)
	budgetRepo := repository.NewBudgetRepository(pool)
	settingsRepo := repository.NewSettingsRepository(pool)
	pendingRepo := repository.NewPendingActionRepository(pool)
	analyticsRepo := repository.NewAnalyticsRepository(pool)

	// Services.
	transactionSvc := service.NewTransactionService(transactionRepo, sourceRepo, categoryRepo)
	sourceSvc := service.NewSourceService(sourceRepo, transactionSvc)
	categorySvc := service.NewCategoryService(categoryRepo)
	billSvc := service.NewBillService(billRepo)
	analyticsSvc := service.NewAnalyticsService(analyticsRepo, categoryRepo, sourceRepo, billRepo, settingsRepo)
	budgetSvc := service.NewBudgetService(budgetRepo, categoryRepo, analyticsSvc)
	overviewSvc := service.NewOverviewService(sourceRepo, billRepo, settingsRepo, analyticsRepo, pendingRepo, transactionSvc)
	badgesSvc := service.NewBadgesService(analyticsRepo, pendingRepo)
	settingsSvc := provideSettingsService(settingsRepo, cfg)
	holdQueueSvc := service.NewHoldQueueService(pendingRepo)

	// Auth.
	tokenSvc := auth.ProvideTokenService(cfg)
	otpSender := auth.ProvideOtpSender(cfg, logger)
	otpRepo := repository.NewOtpRepository(pool)
	otpSvc := auth.ProvideOtpService(otpRepo, otpSender, tokenSvc, cfg)
	authHandler := auth.NewHandler(otpSvc, tokenSvc)
	authMiddleware := auth.ProvideRequireAuth(tokenSvc)

	// Handlers.
	sourceHandler := handler.NewSourceHandler(sourceSvc)
	transactionHandler := handler.NewTransactionHandler(transactionSvc)
	categoryHandler := handler.NewCategoryHandler(categorySvc)
	billHandler := handler.NewBillHandler(billSvc)
	budgetHandler := handler.NewBudgetHandler(budgetSvc)
	overviewHandler := handler.NewOverviewHandler(overviewSvc, badgesSvc)
	analyticsHandler := handler.NewAnalyticsHandler(analyticsSvc)
	holdQueueHandler := handler.NewHoldQueueHandler(holdQueueSvc)
	settingsHandler := handler.NewSettingsHandler(settingsSvc)
	healthHandler := handler.NewHealthHandler()
	versionHandler := handler.NewVersionHandler()

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
		billHandler,
		budgetHandler,
		overviewHandler,
		analyticsHandler,
		holdQueueHandler,
		settingsHandler,
	)

	app := &App{
		Config: cfg,
		Logger: logger,
		Pool:   pool,
		Router: routerRouter,
	}
	return app, nil
}
