// Package config 负责从环境变量加载应用配置。
//
// 配置来源只有环境变量一处，默认值仅保证本地开箱可跑，生产环境必须显式设置。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// 支持的数据库驱动。
const (
	DriverPostgres = "postgres"
	DriverSQLite   = "sqlite"
)

// Config 是应用的全部运行时配置。
type Config struct {
	Env   string // 运行环境：development / test / production
	Debug bool   // 调试开关，决定是否输出完整 SQL，生产必须为 false

	HTTP HTTP
	DB   DB
}

// HTTP 是 HTTP 服务配置。
type HTTP struct {
	Host string
	Port int
}

// Addr 返回监听地址，例如 "0.0.0.0:8080"。
func (h HTTP) Addr() string {
	return fmt.Sprintf("%s:%d", h.Host, h.Port)
}

// DB 是数据库连接配置。
//
// Postgres 与 SQLite 共用本结构，由 Driver 决定实际使用哪些字段。
type DB struct {
	Driver string // postgres 或 sqlite

	// Postgres 专用
	Host     string
	Port     int
	User     string
	Password string
	Name     string
	SSLMode  string

	// SQLite 专用
	Path string

	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// DSN 依据 Driver 返回对应的连接串。
func (d DB) DSN() (string, error) {
	switch d.Driver {
	case DriverPostgres:
		return fmt.Sprintf(
			"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
			d.Host, d.Port, d.User, d.Password, d.Name, d.SSLMode,
		), nil
	case DriverSQLite:
		return d.Path, nil
	default:
		return "", fmt.Errorf("不支持的数据库驱动 %q，可选 %s 或 %s", d.Driver, DriverPostgres, DriverSQLite)
	}
}

// Validate 校验配置，让错误在启动时暴露，而不是在第一次请求时才出现。
func (c Config) Validate() error {
	if _, err := c.DB.DSN(); err != nil {
		return err
	}
	if c.HTTP.Port <= 0 || c.HTTP.Port > 65535 {
		return fmt.Errorf("APP_PORT 非法: %d", c.HTTP.Port)
	}
	if strings.TrimSpace(c.HTTP.Host) == "" {
		return fmt.Errorf("APP_HOST 不能为空")
	}
	if c.DB.Driver == DriverSQLite && strings.TrimSpace(c.DB.Path) == "" {
		return fmt.Errorf("sqlite 驱动必须配置 DB_PATH")
	}
	if c.DB.MaxOpenConns <= 0 {
		return fmt.Errorf("DB_MAX_OPEN_CONNS 必须大于 0")
	}
	if c.DB.MaxIdleConns > c.DB.MaxOpenConns {
		return fmt.Errorf("DB_MAX_IDLE_CONNS(%d) 不能大于 DB_MAX_OPEN_CONNS(%d)", c.DB.MaxIdleConns, c.DB.MaxOpenConns)
	}
	return nil
}

// String 返回脱敏后的配置摘要，避免密码等敏感信息进入日志。
func (c Config) String() string {
	return fmt.Sprintf(
		"env=%s debug=%t http=%s db.driver=%s db.name=%s db.maxOpen=%d",
		c.Env, c.Debug, c.HTTP.Addr(), c.DB.Driver, c.DB.Name, c.DB.MaxOpenConns,
	)
}

// Load 从环境变量读取配置并校验。
func Load() (Config, error) {
	cfg := Config{
		Env:   env("APP_ENV", "development"),
		Debug: envBool("APP_DEBUG", false),
		HTTP: HTTP{
			Host: env("APP_HOST", "0.0.0.0"),
			Port: envInt("APP_PORT", 8080),
		},
		DB: DB{
			Driver: env("DB_DRIVER", DriverSQLite),

			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			Name:     env("DB_NAME", "k_cockpit"),
			SSLMode:  env("DB_SSLMODE", "disable"),

			Path: env("DB_PATH", "data/k_cockpit.db"),

			MaxOpenConns:    envInt("DB_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    envInt("DB_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: envDuration("DB_CONN_MAX_LIFETIME", time.Hour),
		},
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return v
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return v
}
