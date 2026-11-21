package config

import (
	"slices"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("ARGOS_POSTGRES_USER", "argos")
	t.Setenv("ARGOS_POSTGRES_PASSWORD", "p@ss/word")
	t.Setenv("ARGOS_POSTGRES_DB", "argos")
	t.Setenv("ARGOS_SECRET_KEY", "secret")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment != Local || cfg.HTTPPort != 8000 || cfg.JWTAlgorithm != "HS256" ||
		cfg.AccessTokenExpireMinutes != 15 || cfg.RefreshTokenExpireDays != 60 {
		t.Errorf("valores por defecto inesperados: %+v", cfg)
	}
	if !slices.Equal(cfg.CORSOrigins, []string{"*"}) {
		t.Errorf("CORSOrigins = %v", cfg.CORSOrigins)
	}
	if got, want := cfg.DatabaseURL(), "postgres://argos:p%40ss%2Fword@localhost:5432/argos"; got != want {
		t.Errorf("DatabaseURL = %s, se esperaba %s", got, want)
	}
}

func TestLoadRequiresSecrets(t *testing.T) {
	setRequired(t)
	t.Setenv("ARGOS_SECRET_KEY", "")
	if _, err := Load(); err == nil {
		t.Error("ARGOS_SECRET_KEY vacia deberia fallar")
	}
}

func TestLoadParsesValues(t *testing.T) {
	setRequired(t)
	t.Setenv("ARGOS_ENVIRONMENT", "production")
	t.Setenv("ARGOS_CORS_ORIGINS", "https://a.example,https://b.example")
	t.Setenv("ARGOS_POSTGRES_PORT", "5433")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsProduction() || cfg.PostgresPort != 5433 {
		t.Errorf("valores inesperados: %+v", cfg)
	}
	if !slices.Equal(cfg.CORSOrigins, []string{"https://a.example", "https://b.example"}) {
		t.Errorf("CORSOrigins = %v", cfg.CORSOrigins)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	for name, env := range map[string][2]string{
		"entorno":   {"ARGOS_ENVIRONMENT", "dev"},
		"algoritmo": {"ARGOS_JWT_ALGORITHM", "RS256"},
		"duracion":  {"ARGOS_ACCESS_TOKEN_EXPIRE_MINUTES", "0"},
		"pool":      {"ARGOS_DB_MAX_CONNS", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			t.Setenv(env[0], env[1])
			if _, err := Load(); err == nil {
				t.Errorf("%s=%s deberia fallar", env[0], env[1])
			}
		})
	}
}
