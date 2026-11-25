// Package config carga la configuracion desde variables de entorno.
//
// ARCHITECTURE.md prohibe el uso de `.env.example`: la configuracion se gestiona
// dinamicamente segun el entorno de despliegue. En local se admite un `.env`
// (no versionado); en despliegue las variables vienen del entorno del
// contenedor y tienen prioridad sobre el `.env`.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"slices"
	"strconv"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Environment es el entorno de ejecucion.
type Environment string

// Entornos de ejecucion soportados.
const (
	Local      Environment = "local"
	Testing    Environment = "testing"
	Staging    Environment = "staging"
	Production Environment = "production"
)

// Config es la configuracion central de la aplicacion. Todas las variables
// usan el prefijo ARGOS_. Las credenciales no tienen valor por defecto: si
// faltan, la aplicacion no arranca.
type Config struct {
	Environment Environment `env:"ENVIRONMENT" envDefault:"local"`
	Debug       bool        `env:"DEBUG" envDefault:"false"`
	HTTPPort    int         `env:"HTTP_PORT" envDefault:"8000"`

	// --- Base de datos PostgreSQL ---
	PostgresHost     string `env:"POSTGRES_HOST" envDefault:"localhost"`
	PostgresPort     int    `env:"POSTGRES_PORT" envDefault:"5432"`
	PostgresUser     string `env:"POSTGRES_USER,required,notEmpty"`
	PostgresPassword string `env:"POSTGRES_PASSWORD,required,notEmpty"`
	PostgresDB       string `env:"POSTGRES_DB,required,notEmpty"`
	// DBEcho registra cada sentencia SQL en el log (nivel DEBUG).
	DBEcho     bool  `env:"DB_ECHO" envDefault:"false"`
	DBMaxConns int32 `env:"DB_MAX_CONNS" envDefault:"30"`

	// --- Seguridad / JWT ---
	SecretKey                string `env:"SECRET_KEY,required,notEmpty"`
	JWTAlgorithm             string `env:"JWT_ALGORITHM" envDefault:"HS256"`
	AccessTokenExpireMinutes int    `env:"ACCESS_TOKEN_EXPIRE_MINUTES" envDefault:"15"`
	RefreshTokenExpireDays   int    `env:"REFRESH_TOKEN_EXPIRE_DAYS" envDefault:"60"`

	// --- CORS --- Lista separada por comas; "*" admite cualquier origen.
	CORSOrigins []string `env:"CORS_ORIGINS" envDefault:"*" envSeparator:","`

	// --- Cifrado de campos (AES-256-GCM): secreto TOTP y, en fase 2, accesos ---
	// 32 bytes en base64. En local y testing, si falta, se deriva de SECRET_KEY.
	FieldEncryptionKey string `env:"FIELD_ENCRYPTION_KEY"`

	// --- Enlaces en correos ---
	WebBaseURL string `env:"WEB_BASE_URL" envDefault:"http://localhost:4200"`

	// --- AWS (S3 y SES). Credenciales por la cadena estandar del SDK, salvo
	// el emulador local, que usa S3AccessKey y S3SecretKey. ---
	AWSRegion string `env:"AWS_REGION" envDefault:"us-east-1"`
	S3Bucket  string `env:"S3_BUCKET"`
	// S3EndpointURL solo en local (emulador S3); vacio usa AWS.
	S3EndpointURL string `env:"S3_ENDPOINT_URL"`
	// S3PublicEndpointURL es el host con el que se firman las URLs que abre el
	// navegador: en local el emulador se ve como localhost fuera de Docker.
	S3PublicEndpointURL string `env:"S3_PUBLIC_ENDPOINT_URL"`
	S3AccessKey         string `env:"S3_ACCESS_KEY"`
	S3SecretKey         string `env:"S3_SECRET_KEY"`

	// --- Correo ---
	// MailDriver: "log" escribe cada correo en el log y en MailDir (local y
	// testing); "ses" lo envia por Amazon SES.
	MailDriver string `env:"MAIL_DRIVER"`
	MailDir    string `env:"MAIL_DIR" envDefault:"tmp/mail"`
	SESFrom    string `env:"SES_FROM" envDefault:"ARGOS <no-reply@argos.gt>"`

	// --- Cola de trabajos ---
	WorkerConcurrency int `env:"WORKER_CONCURRENCY" envDefault:"4"`

	// --- Fase 6: limites y facturacion ---
	// Peticiones por IP y minuto en acceso (login, clave, refresh) y en las
	// rutas publicas, y por clave API y minuto en POST /public/reservations.
	// 0 = sin limite.
	RateLimitAuth   int `env:"RATE_LIMIT_AUTH" envDefault:"60"`
	RateLimitPublic int `env:"RATE_LIMIT_PUBLIC" envDefault:"300"`
	RateLimitAPIKey int `env:"RATE_LIMIT_API_KEY" envDefault:"60"`
	// Pasarela de pago: fake (simulada) hasta elegir proveedor (avance.md).
	BillingProvider string `env:"BILLING_PROVIDER" envDefault:"fake"`
	// Secreto HMAC de los webhooks de la pasarela; vacio = derivado de SECRET_KEY.
	BillingWebhookSecret string `env:"BILLING_WEBHOOK_SECRET"`
	// Mapa real de la web (Google Maps, mapa 3D fotorrealista). Clave de
	// navegador restringida por dominio; vacia = la web usa el mapa ilustrado.
	GoogleMapsKey   string `env:"GOOGLE_MAPS_KEY"`
	GoogleMapsMapID string `env:"GOOGLE_MAPS_MAP_ID"`
}

// Drivers de correo.
const (
	MailLog = "log"
	MailSES = "ses"
)

// Mail devuelve el driver de correo efectivo: el configurado o, si falta, log
// en local y testing y ses en el resto.
func (c Config) Mail() string {
	if c.MailDriver != "" {
		return c.MailDriver
	}
	if c.IsDevelopment() {
		return MailLog
	}
	return MailSES
}

// IsDevelopment indica si corre en local o en pruebas.
func (c Config) IsDevelopment() bool {
	return c.Environment == Local || c.Environment == Testing
}

var supportedJWTAlgorithms = []string{"HS256", "HS384", "HS512"}

// Load lee el `.env` local (si existe) y despues las variables de entorno.
func Load() (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("leyendo .env: %w", err)
	}
	cfg, err := env.ParseAsWithOptions[Config](env.Options{Prefix: "ARGOS_"})
	if err != nil {
		return Config{}, fmt.Errorf("configuracion invalida: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch c.Environment {
	case Local, Testing, Staging, Production:
	default:
		return fmt.Errorf("ARGOS_ENVIRONMENT invalido: %q (local, testing, staging o production)", c.Environment)
	}
	if !slices.Contains(supportedJWTAlgorithms, c.JWTAlgorithm) {
		return fmt.Errorf("ARGOS_JWT_ALGORITHM invalido: %q (HS256, HS384 o HS512)", c.JWTAlgorithm)
	}
	if c.AccessTokenExpireMinutes <= 0 || c.RefreshTokenExpireDays <= 0 {
		return errors.New("la duracion de los tokens debe ser positiva")
	}
	if c.DBMaxConns <= 0 {
		return errors.New("ARGOS_DB_MAX_CONNS debe ser positivo")
	}
	if c.WorkerConcurrency <= 0 {
		return errors.New("ARGOS_WORKER_CONCURRENCY debe ser positivo")
	}
	switch c.MailDriver {
	case "", MailLog, MailSES:
	default:
		return fmt.Errorf("ARGOS_MAIL_DRIVER invalido: %q (log o ses)", c.MailDriver)
	}
	if !c.IsDevelopment() && (c.FieldEncryptionKey == "" || c.S3Bucket == "") {
		return errors.New("ARGOS_FIELD_ENCRYPTION_KEY y ARGOS_S3_BUCKET son obligatorias fuera de local y testing")
	}
	return nil
}

// DatabaseURL devuelve la URL de conexion a PostgreSQL con las credenciales
// escapadas.
func (c Config) DatabaseURL() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.PostgresUser, c.PostgresPassword),
		Host:   net.JoinHostPort(c.PostgresHost, strconv.Itoa(c.PostgresPort)),
		Path:   "/" + c.PostgresDB,
	}
	return u.String()
}

// IsProduction indica si la aplicacion corre en produccion.
func (c Config) IsProduction() bool {
	return c.Environment == Production
}
