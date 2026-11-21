package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/middleware"
	"github.com/cko-recruitment/payment-gateway-challenge-go/internal/telemetry"
	"gopkg.in/yaml.v3"
)

var environmentName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

type Config struct {
	Server         ServerConfig         `yaml:"server"`
	Authentication AuthenticationConfig `yaml:"authentication"`
	BankSimulator  BankSimulatorConfig  `yaml:"bank_simulator"`
	Reconciliation ReconciliationConfig `yaml:"reconciliation"`
	Observability  telemetry.Options    `yaml:"observability"`
	RateLimit      RateLimitConfig      `yaml:"rate_limit"`
}

type RateLimitConfig struct {
	RequestsPerSecond integer32 `yaml:"requests_per_second"`
}

type ServerConfig struct {
	Address           string        `yaml:"address"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
}

func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func (s ServerConfig) Validate() error {
	const maxDuration = (1<<31 - 1) * time.Millisecond
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"read_header_timeout", s.ReadHeaderTimeout},
		{"read_timeout", s.ReadTimeout},
		{"write_timeout", s.WriteTimeout},
		{"idle_timeout", s.IdleTimeout},
	} {
		if field.value < time.Millisecond || field.value > maxDuration {
			return fmt.Errorf("server.%s must be between 1 and 2147483647 milliseconds", field.name)
		}
	}
	if s.ReadHeaderTimeout > s.ReadTimeout {
		return errors.New("server.read_timeout must be at least server.read_header_timeout")
	}
	if s.WriteTimeout <= s.ReadTimeout {
		return errors.New("server.write_timeout must exceed server.read_timeout")
	}
	return nil
}

type AuthenticationConfig struct {
	APIKeys []middleware.APIKeyCredential `yaml:"api_keys"`
}

type BankSimulatorConfig struct {
	BaseAddress string        `yaml:"base_address"`
	Timeout     time.Duration `yaml:"timeout"`
}

type ReconciliationConfig struct {
	SweepInterval                      time.Duration `yaml:"sweep_interval"`
	BankTransactionStatusUpdateTimeout time.Duration `yaml:"bank_transaction_status_update_timeout"`
	BatchSize                          integer32     `yaml:"batch_size"`
}

type integer32 int32

// YAML otherwise permits fractional numbers to be truncated into integer fields.
func (i *integer32) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag != "!!int" {
		return errors.New("value must be a 32-bit integer")
	}
	var value int32
	if err := node.Decode(&value); err != nil {
		return errors.New("value must be a 32-bit integer")
	}
	*i = integer32(value)
	return nil
}

func Load() (*Config, error) {
	environment, configured := os.LookupEnv("ENVIRONMENT")
	if !configured {
		environment = "development"
	}
	if !environmentName.MatchString(environment) {
		return nil, errors.New("ENVIRONMENT must start with a lowercase letter and contain only lowercase letters, digits, '_' or '-'")
	}
	filename := "config." + environment + ".yaml"
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open configuration %s: %w", filename, err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	cfg := Config{
		Server:    DefaultServerConfig(),
		RateLimit: RateLimitConfig{RequestsPerSecond: 100},
	}
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode configuration %s: %w", filename, err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("decode configuration %s: %w", filename, err)
		}
		return nil, fmt.Errorf("configuration %s must contain exactly one YAML document", filename)
	}
	_, port, err := net.SplitHostPort(cfg.Server.Address)
	if err != nil {
		return nil, errors.New("server.address must be a host:port address, such as ':8090'")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return nil, errors.New("server.address must contain a numeric port between 0 and 65535")
	}
	if err := cfg.Server.Validate(); err != nil {
		return nil, err
	}
	if cfg.BankSimulator.Timeout >= cfg.Server.WriteTimeout-cfg.Server.ReadTimeout {
		return nil, errors.New("server.write_timeout must exceed server.read_timeout plus bank_simulator.timeout")
	}
	return &cfg, nil
}
