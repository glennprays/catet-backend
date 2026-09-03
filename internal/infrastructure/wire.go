//go:build wireinject
// +build wireinject

package infrastructure

import (
	"github.com/google/wire"

	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/handler"
	"github.com/glennprays/catet-backend/internal/router"
	"github.com/glennprays/catet-backend/pkg/logger"
)

var CoreSet = wire.NewSet(
	config.Load,
	logger.ProviderLogger,
)

var ApiSet = wire.NewSet(
	handler.NewHealthHandler,
	handler.NewVersionHandler,
	router.NewRouter,
)

func InitializeApp() (*App, error) {
	wire.Build(
		CoreSet,
		ApiSet,
		wire.Struct(new(App), "*"),
	)
	return nil, nil
}
