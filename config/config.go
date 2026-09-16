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

	// Auth: HMAC secret for session JWTs. A dev default exists so the
	// stack boots without env setup; production refuses it (see Load).
	AuthSecret string `mapstructure:"AUTH_SECRET" default:"dev-only-secret" validate:"required"`
	// Bootstrap claim: the phone that takes over the pre-seeded admin
	// account on the next boot. Set it once, boot, then remove it from
	// the environment — an already-claimed admin ignores it, and empty
	// is the normal steady state.
	OwnerPhone string `mapstructure:"OWNER_PHONE"`
	// Bootstrap claim, optional companion of OWNER_PHONE: WhatsApp
	// increasingly addresses contacts by an opaque "@lid" instead of a
	// phone number. Set it (see GET /contact/check) when the gateway
	// cannot resolve the lid itself; empty lets the claim look it up.
	// Remove it together with OWNER_PHONE after the first boot.
	OwnerLid string `mapstructure:"OWNER_LID"`
	// Base URL of the dashboard, used to build the invite and password
	// reset links the bot sends over WhatsApp.
	DashboardURL string `mapstructure:"DASHBOARD_URL" default:"http://localhost:3001" validate:"required,url"`

	// OTP lifetime in seconds (5 minutes by default).
	OtpTTLSeconds int `mapstructure:"OTP_TTL_SECONDS" default:"300" validate:"required,min=30,max=3600"`

	// WhatsApp gateway (glennprays/whatsapp-gateway). Empty URL selects
	// the log sender — the dev default, where replies and OTP codes are
	// printed to the API log instead of being delivered.
	WaGatewayURL   string `mapstructure:"WA_GATEWAY_URL"`   // e.g. https://waga.example.com/api/v1
	WaGatewayToken string `mapstructure:"WA_GATEWAY_TOKEN"` // pre-issued phone-scoped JWT
	// HMAC secret shared with the gateway subscription. Empty leaves the
	// public webhook route unregistered.
	WaWebhookSecret string `mapstructure:"WA_WEBHOOK_SECRET"`
	// Public URL of POST /api/v1/webhooks/whatsapp. Empty = no
	// self-registration at boot (register the subscription by hand).
	WaWebhookURL string `mapstructure:"WA_WEBHOOK_URL"`

	// Master switch for the daily bill-reminder digest (ADR-0006). The
	// per-bill opt-in is the user's control; this is the operator's, and
	// it exists so a second API instance can be run without doubling the
	// only outbound path the bot has.
	BillRemindersEnabled bool `mapstructure:"BILL_REMINDERS_ENABLED" default:"true"`

	// OpenAI-compatible chat-completions endpoint used for category
	// guessing, Source alias resolution and the optional rekap verdict.
	// Empty API key disables the LLM entirely: captures land
	// uncategorised in needs-review and alias misses ask instead.
	LlmBaseURL        string `mapstructure:"LLM_BASE_URL" default:"https://api.openai.com/v1"`
	LlmAPIKey         string `mapstructure:"LLM_API_KEY"`
	LlmModel          string `mapstructure:"LLM_MODEL" default:"gpt-4o-mini"`
	LlmTimeoutSecs    int    `mapstructure:"LLM_TIMEOUT_SECONDS" default:"8" validate:"min=1,max=60"`
	LlmVerdictEnabled bool   `mapstructure:"LLM_VERDICT_ENABLED" default:"false"`
	// Receipt photos go to a vision-capable model, which is slower and
	// need not be the text model. Empty = reuse LLM_MODEL.
	LlmVisionModel       string `mapstructure:"LLM_VISION_MODEL"`
	LlmVisionTimeoutSecs int    `mapstructure:"LLM_VISION_TIMEOUT_SECONDS" default:"45" validate:"min=1,max=120"`
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
	// Production must not run on the dev-only defaults: a public
	// deployment with the shared dev secret would be trivially
	// compromisable, and localhost links in a WhatsApp invite are dead
	// on arrival.
	if cfg.Env == PROD {
		if cfg.AuthSecret == "dev-only-secret" {
			return nil, fmt.Errorf("invalid configuration: AUTH_SECRET must be set in production")
		}
		if cfg.DashboardURL == "http://localhost:3001" {
			return nil, fmt.Errorf("invalid configuration: DASHBOARD_URL must be set in production")
		}
		// An unsigned public webhook route would let anyone write to the
		// ledger; refuse the combination instead of silently disabling it.
		if cfg.WaGatewayURL != "" && cfg.WaWebhookSecret == "" {
			return nil, fmt.Errorf("invalid configuration: WA_WEBHOOK_SECRET must be set when WA_GATEWAY_URL is set in production")
		}
	}

	// Production must explicitly enumerate origins — a wildcard there
	// is almost always a mistake.
	if cfg.Env == PROD && cfg.CorsAllowedOrigins == "*" {
		return nil, fmt.Errorf("invalid configuration: CORS_ALLOWED_ORIGINS must be set to specific origins in production, got %q", cfg.CorsAllowedOrigins)
	}

	return cfg, nil
}
