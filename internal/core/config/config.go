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
	AccessTokenExpireMinutes int    `env:"ACCESS_TOKEN_EXPIRE_MINUTES" envDefault:"30"`
	RefreshTokenExpireDays   int    `env:"REFRESH_TOKEN_EXPIRE_DAYS" envDefault:"7"`

	// --- CORS --- Lista separada por comas; "*" admite cualquier origen.
	CORSOrigins []string `env:"CORS_ORIGINS" envDefault:"*" envSeparator:","`
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
