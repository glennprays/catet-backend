package infrastructure

import (
	"context"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/llm"
	"github.com/glennprays/catet-backend/internal/secretbox"
	"github.com/glennprays/catet-backend/internal/service"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProvidePool opens the pgx pool for the process lifetime. Live in a
// non-wireinject file so both the injector and the hand-maintained
// wire_gen.go see it.
func ProvidePool(cfg *config.Config) (*pgxpool.Pool, error) {
	return NewPool(context.Background(), cfg)
}

// provideWebhookVerifier builds the HMAC verifier for the public
// gateway webhook. The secret is the route's only authentication, so an
// empty one leaves the route unmounted (see router.Setup).
func provideWebhookVerifier(cfg *config.Config) *waga.WebhookVerifier {
	return waga.NewWebhookVerifier(cfg.WaWebhookSecret)
}

// provideSecretBox derives the at-rest cipher for the stored LLM API
// key from AUTH_SECRET. One secret to protect, not two — and rotating
// it invalidates the stored key on purpose (see internal/secretbox).
func provideSecretBox(cfg *config.Config) secretbox.Box {
	return secretbox.New(cfg.AuthSecret)
}

// provideLLMClient points the bot's client at the provider service
// rather than at frozen env values, which is the whole no-restart
// story: an admin saving /admin/llm changes the next request.
func provideLLMClient(svc *service.LLMProviderService) *llm.Client {
	return llm.NewWithSource(svc)
}
