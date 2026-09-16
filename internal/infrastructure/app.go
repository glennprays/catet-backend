package infrastructure

import (
	"github.com/glennprays/catet-backend/config"
	"github.com/glennprays/catet-backend/internal/bot"
	"github.com/glennprays/catet-backend/internal/gateway"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/router"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/log"
	"github.com/jackc/pgx/v5/pgxpool"
)

type App struct {
	Config *config.Config
	Logger *log.Logger
	Pool   *pgxpool.Pool
	Router *router.Router
	// Bot is started by main once the process is up: it registers the
	// webhook subscription and replays anything a crash left mid-flight.
	Bot *bot.Harness
	// Users and Gateway serve the one-time bootstrap claim main runs
	// before listening: turning the migration's placeholder admin into a
	// real account needs the repository and, for the lid lookup, the
	// gateway.
	Users   *repository.UserRepository
	Gateway gateway.Sender
	// Reminders is the daily bill digest ticker (ADR-0006), started by
	// main and cancelled on shutdown.
	Reminders *service.ReminderService
}
