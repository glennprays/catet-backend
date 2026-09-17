//go:build wireinject
// +build wireinject

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
	"github.com/google/wire"
)

var CoreSet = wire.NewSet(
	config.Load,
	logger.ProviderLogger,
	ProvidePool,
	gateway.Provide,
)

var RepositorySet = wire.NewSet(
	repository.NewSourceRepository,
	repository.NewTransactionRepository,
	repository.NewCategoryRepository,
	repository.NewMerchantRepository,
	repository.NewBillRepository,
	repository.NewBillPaymentRepository,
	repository.NewBillReminderSendRepository,
	repository.NewBudgetRepository,
	repository.NewSettingsRepository,
	repository.NewPendingActionRepository,
	repository.NewAnalyticsRepository,
	repository.NewOtpRepository,
	repository.NewInboundMessageRepository,
	repository.NewBotChatStateRepository,
	repository.NewUserRepository,
	repository.NewInviteRepository,
	repository.NewPasswordResetRepository,
	repository.NewLLMProviderRepository,
	wire.Bind(new(auth.CodeStore), new(*repository.OtpRepository)),
)

var ServiceSet = wire.NewSet(
	service.NewTransactionService,
	service.NewSourceService,
	service.NewCategoryService,
	service.NewMerchantService,
	service.NewBillService,
	service.NewBudgetService,
	service.NewAnalyticsService,
	service.NewOverviewService,
	service.NewBadgesService,
	service.NewSettingsService,
	service.NewReminderService,
	service.NewHoldQueueService,
	service.NewOnboardingService,
	service.NewLLMProviderService,
	provideSecretBox,
)

var AuthSet = wire.NewSet(
	auth.ProvideTokenService,
	auth.ProvideOtpSender,
	auth.ProvideOtpService,
	auth.ProvideAuthService,
	auth.ProvideInviteService,
	auth.ProvideRequireAuth,
	auth.NewHandler,
)

var ApiSet = wire.NewSet(
	handler.NewHealthHandler,
	handler.NewVersionHandler,
	handler.NewSourceHandler,
	handler.NewTransactionHandler,
	handler.NewCategoryHandler,
	handler.NewMerchantHandler,
	handler.NewBillHandler,
	handler.NewBudgetHandler,
	handler.NewOverviewHandler,
	handler.NewAnalyticsHandler,
	handler.NewHoldQueueHandler,
	handler.NewSettingsHandler,
	handler.NewUserHandler,
	handler.NewAdminLLMHandler,
	handler.NewWebhookHandler,
	router.NewRouter,
)

var BotSet = wire.NewSet(
	provideLLMClient,
	bot.NewHarness,
	provideWebhookVerifier,
	wire.Bind(new(service.PendingExecutor), new(*bot.Harness)),
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
		BotSet,
		ApiSet,
		wire.Struct(new(App), "*"),
	)
	return nil, nil
}
