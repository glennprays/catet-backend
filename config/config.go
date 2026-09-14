package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/creasty/defaults"
	"github.com/go-playground/validator/v10"
	"github.com/joho/godotenv"
	"github.com/spf13/viper"
)

type Config struct {
	Env     Environment `mapstructure:"ENV" default:"development" validate:"required,oneof=development staging production"`
	AppName string      `mapstructure:"APP_NAME" default:"catet-backend" validate:"required"`
	AppPort int         `mapstructure:"APP_PORT" default:"3000" validate:"required,min=1,max=65535"`

	LogLevel  string `mapstructure:"LOG_LEVEL" default:"debug" validate:"required"`
	LogOutput string `mapstructure:"LOG_OUTPUT" default:"stdout" validate:"required"`

	DBHost     string `mapstructure:"DB_HOST" default:"localhost" validate:"required"`
	DBPort     int    `mapstructure:"DB_PORT" default:"5432" validate:"required,min=1,max=65535"`
	DBUser     string `mapstructure:"DB_USER" default:"postgres" validate:"required"`
	DBPassword string `mapstructure:"DB_PASSWORD" validate:"required"`
	DBName     string `mapstructure:"DB_NAME" validate:"required"`

	// Comma-separated list of allowed origins for CORS. Development should
	// enumerate local dashboard origins; production refuses wildcard origins.
	CorsAllowedOrigins string `mapstructure:"CORS_ALLOWED_ORIGINS" default:"http://localhost:3001" validate:"required"`
	// Postgres connection string for pgxpool. Compose-based dev also
	// exposes discrete DB_* vars; the pool consumes the URL form.
	DatabaseURL string `mapstructure:"DATABASE_URL" default:"postgres://postgres:catet_dev@localhost:5432/catet_dev?sslmode=disable" validate:"required"`

	// Auth: HMAC secret for session JWTs and the single owner phone.
	// Dev defaults exist so the stack boots without env setup; production
	// refuses both defaults (see Load).
	AuthSecret string `mapstructure:"AUTH_SECRET" default:"dev-only-secret" validate:"required"`
	OwnerPhone string `mapstructure:"OWNER_PHONE" default:"+628000000000" validate:"required"`

	// OTP lifetime in seconds (5 minutes by default).
	OtpTTLSeconds int `mapstructure:"OTP_TTL_SECONDS" default:"300" validate:"required,min=30,max=3600"`

	// WhatsApp gateway delivery for OTP codes. Empty URL selects the
	// log sender (codes printed to the API log) — the dev default.
	WaGatewayURL   string `mapstructure:"WA_GATEWAY_URL"`
	WaGatewayToken string `mapstructure:"WA_GATEWAY_TOKEN"`
}

type Environment string

const (
	DEV     Environment = "development"
	STAGING Environment = "staging"
	PROD    Environment = "production"
)

func (e Environment) String() string {
	return string(e)
}

func Load() (*Config, error) {
	cfg := &Config{}

	// Apply defaults from struct tags
	if err := defaults.Set(cfg); err != nil {
		return nil, err
	}

	envStr := strings.ToLower(os.Getenv("ENV"))
	env := Environment(envStr)
	if env == "" {
		env = DEV
	}

	// Load .env file in development
	if env == DEV {
		_ = godotenv.Load(".env")
	}

	// Configure Viper to read from environment variables
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Auto-bind each struct field by key
	t := reflect.TypeOf(cfg).Elem()
	for i := range t.NumField() {
		field := t.Field(i)
		key := field.Tag.Get("mapstructure")
		if key != "" {
			if err := viper.BindEnv(key); err != nil {
				return nil, err
			}
		}
	}

	// Unmarshal environment variables into config
	// This will override defaults with actual env values
	if err := viper.Unmarshal(cfg); err != nil {
		return nil, err
	}

	// Validate required fields and enum constraints. This catches
	// missing DB_PASSWORD / DB_NAME at boot rather than at first query.
	if err := validator.New().Struct(cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	// Production must not run on the dev-only auth defaults: a public
	// deployment with the shared dev secret or the placeholder owner
	// phone would be trivially compromisable.
	if cfg.Env == PROD {
		if cfg.AuthSecret == "dev-only-secret" {
			return nil, fmt.Errorf("invalid configuration: AUTH_SECRET must be set in production")
		}
		if cfg.OwnerPhone == "+628000000000" {
			return nil, fmt.Errorf("invalid configuration: OWNER_PHONE must be set in production")
		}
	}

	// Production must explicitly enumerate origins — a wildcard there
	// is almost always a mistake.
	if cfg.Env == PROD && cfg.CorsAllowedOrigins == "*" {
		return nil, fmt.Errorf("invalid configuration: CORS_ALLOWED_ORIGINS must be set to specific origins in production, got %q", cfg.CorsAllowedOrigins)
	}

	return cfg, nil
}
