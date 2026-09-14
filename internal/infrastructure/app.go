package infrastructure

import (
	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/router"
	"github.com/glennprays/log"
	"github.com/jackc/pgx/v5/pgxpool"
)

type App struct {
	Config *config.Config
	Logger *log.Logger
	Pool   *pgxpool.Pool
	Router *router.Router
}
