package collector

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr          string
	DatabaseURL         string
	Tokens              []string
	MinSupportedVersion string
	MaxBodyBytes        int64
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	ShutdownTimeout     time.Duration
}

func ConfigFromEnv() (Config, error) {
	c := Config{ListenAddr: ":8080", MinSupportedVersion: "1.0.0", MaxBodyBytes: 64 << 20, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, ShutdownTimeout: 15 * time.Second}
	if v := os.Getenv("COLLECTOR_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	c.DatabaseURL = os.Getenv("DATABASE_URL")
	for _, token := range strings.Split(os.Getenv("COLLECTOR_TOKENS"), ",") {
		if token = strings.TrimSpace(token); token != "" {
			c.Tokens = append(c.Tokens, token)
		}
	}
	if v := os.Getenv("MIN_SUPPORTED_VERSION"); v != "" {
		c.MinSupportedVersion = v
	}
	if v := os.Getenv("MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1024 {
			return c, fmt.Errorf("MAX_BODY_BYTES must be an integer >= 1024")
		}
		c.MaxBodyBytes = n
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if len(c.Tokens) == 0 {
		return c, errors.New("COLLECTOR_TOKENS must contain at least one token")
	}
	return c, nil
}
