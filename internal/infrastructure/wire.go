//go:build wireinject
// +build wireinject

package infrastructure

import (
	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/auth"
	"github.com/glennprays/catet-backend/internal/handler"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/router"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/pkg/logger"
	"github.com/google/wire"
)

var CoreSet = wire.NewSet(
	config.Load,
	logger.ProviderLogger,
	ProvidePool,
)

var RepositorySet = wire.NewSet(
	repository.NewSourceRepository,
	repository.NewTransactionRepository,
	repository.NewCategoryRepository,
	repository.NewBillRepository,
	repository.NewBudgetRepository,
	repository.NewSettingsRepository,
	repository.NewPendingActionRepository,
	repository.NewAnalyticsRepository,
	repository.NewOtpRepository,
	wire.Bind(new(auth.CodeStore), new(*repository.OtpRepository)),
)

var ServiceSet = wire.NewSet(
	service.NewTransactionService,
	service.NewSourceService,
	service.NewCategoryService,
	service.NewBillService,
	service.NewBudgetService,
	service.NewAnalyticsService,
	service.NewOverviewService,
	service.NewBadgesService,
	service.NewSettingsService,
	service.NewHoldQueueService,
	provideSettingsService,
)

var AuthSet = wire.NewSet(
	auth.ProvideTokenService,
	auth.ProvideOtpSender,
	auth.ProvideOtpService,
	auth.ProvideRequireAuth,
	auth.NewHandler,
)

var ApiSet = wire.NewSet(
	handler.NewHealthHandler,
	handler.NewVersionHandler,
	handler.NewSourceHandler,
	handler.NewTransactionHandler,
	handler.NewCategoryHandler,
	handler.NewBillHandler,
	handler.NewBudgetHandler,
	handler.NewOverviewHandler,
	handler.NewAnalyticsHandler,
	handler.NewHoldQueueHandler,
	handler.NewSettingsHandler,
	router.NewRouter,
)

// InitializeApp is the Wire injector; the compiled graph lives in
// wire_gen.go (hand-maintained while wire cannot parse Go 1.25 export
// data — see the note there).
func InitializeApp() (*App, error) {
	wire.Build(
		CoreSet,
		RepositorySet,
		ServiceSet,
		AuthSet,
		ApiSet,
		wire.Struct(new(App), "*"),
	)
	return nil, nil
}
