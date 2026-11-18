package e2e

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type configuration struct {
	BaseURL        string `yaml:"base_url"`
	APIKey         string `yaml:"api_key"`
	OtherAPIKey    string `yaml:"other_api_key"`
	ReadOnlyAPIKey string `yaml:"read_only_api_key"`
}

func loadConfig(filename string) (*configuration, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open E2E configuration: %w", err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var cfg configuration
	if err := decoder.Decode(&cfg); err != nil {
		// YAML decoder errors can include credential values.
		return nil, errors.New("invalid E2E YAML: expected base_url, api_key, other_api_key and read_only_api_key")
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("E2E configuration must contain exactly one YAML document")
	}
	for _, field := range []struct{ name, value string }{
		{"base_url", cfg.BaseURL}, {"api_key", cfg.APIKey},
		{"other_api_key", cfg.OtherAPIKey}, {"read_only_api_key", cfg.ReadOnlyAPIKey},
	} {
		if strings.TrimSpace(field.value) == "" {
			return nil, fmt.Errorf("E2E configuration requires %s", field.name)
		}
	}
	return &cfg, nil
}
