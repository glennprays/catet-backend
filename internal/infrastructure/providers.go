package infrastructure

import (
	"context"

	"github.com/glennprays/catet-backend/config"
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
