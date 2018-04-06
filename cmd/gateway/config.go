package main

import (
	envstruct "code.cloudfoundry.org/go-envstruct"
	"code.cloudfoundry.org/log-cache/internal/tls"
)

// Config is the configuration for a LogCache Gateway.
type Config struct {
	Addr            string `env:"ADDR, required"`
	LogCacheAddr    string `env:"LOG_CACHE_ADDR, required"`
	GroupReaderAddr string `env:"GROUP_READER_ADDR, required"`
	HealthPort      int    `env:"HEALTH_PORT"`
	TLS             tls.TLS
}

// LoadConfig creates Config object from environment variables
func LoadConfig() (*Config, error) {
	c := Config{
		Addr:            ":8081",
		HealthPort:      6063,
		LogCacheAddr:    "localhost:8080",
		GroupReaderAddr: "localhost:8082",
	}

	if err := envstruct.Load(&c); err != nil {
		return nil, err
	}

	return &c, nil
}
