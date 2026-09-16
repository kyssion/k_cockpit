// Package config 负责从环境变量加载应用配置。
//
// 配置来源只有环境变量一处，默认值仅保证本地开箱可跑，生产环境必须显式设置。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// 运行环境取值。
const (
	EnvDevelopment = "development"
	EnvTest        = "test"
	EnvProduction  = "production"
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

	HTTP     HTTP
	DB       DB
	Session  Session
	Agent    Agent
	Security Security
}

// Security 是高风险操作防护相关的配置。
type Security struct {
	// DevBypassCode 是**开发期万能验证码**。
	//
	// 非空时，TOTP 绑定确认与二次验证都额外接受这个固定值。它存在的唯一
	// 理由是开发阶段不必每 30 秒掏一次手机。
	//
	// 生产环境配置它会导致**启动失败**，不是静默忽略：静默忽略会让部署者
	// 以为自己配的东西没生效（或者更糟——以为生效了但其实没有），而启动
	// 失败会强制他面对这件事。见 Validate。
	//
	// 这不是「弱一点的验证」，而是**没有验证**：任何知道这个值的人都能通过
	// 高风险操作的门禁。因此它的每一次命中都会写审计，且启动时会打印警告。
	DevBypassCode string
}

// 节点 agent 通道的传输方式。
const (
	// AgentTransportMock 开发期使用：接口直接返回假数据，不连接真实节点。
	AgentTransportMock = "mock"
	// AgentTransportGRPC 真实实现（gRPC 双向流），尚未开发。
	AgentTransportGRPC = "grpc"
)

// Agent 是控制面与节点 agent 之间通道的配置。
type Agent struct {
	// Transport 在**启动时**决定，运行期不可切换（ADR-0007）。
	// 同一进程内不得对不同节点使用不同实现，否则会出现无法复现的中间状态。
	Transport string
}

// Session 是认证会话配置。
type Session struct {
	// Secret 是令牌签名密钥。轮换它会让全部既有令牌立即失效（f-1-01 R-012）。
	Secret string
	// SecureCookie 决定会话 Cookie 是否带 Secure 标记。
	// HTTPS 部署必须为 true；本地 HTTP 开发必须为 false，否则浏览器不回传 Cookie。
	SecureCookie bool
	// IdleTimeout 是空闲超时，从最后一次真实用户活动起算。
	IdleTimeout time.Duration
	// AbsoluteTimeout 是会话绝对上限，从签发起算。
	AbsoluteTimeout time.Duration
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
	// 生产环境必须有固定的签名密钥：随机密钥会让重启后全部会话失效，
	// 也意味着多实例之间无法互认令牌。
	if c.Env == EnvProduction && strings.TrimSpace(c.Session.Secret) == "" {
		return fmt.Errorf("生产环境必须配置 SESSION_SECRET")
	}
	if c.Session.IdleTimeout <= 0 || c.Session.AbsoluteTimeout <= 0 {
		return fmt.Errorf("会话超时必须为正数")
	}
	if c.Session.IdleTimeout > c.Session.AbsoluteTimeout {
		return fmt.Errorf("空闲超时(%s)不能大于绝对上限(%s)", c.Session.IdleTimeout, c.Session.AbsoluteTimeout)
	}
	if c.Agent.Transport != AgentTransportMock && c.Agent.Transport != AgentTransportGRPC {
		return fmt.Errorf("AGENT_TRANSPORT 非法: %q，可选 %s 或 %s",
			c.Agent.Transport, AgentTransportMock, AgentTransportGRPC)
	}
	// 开发期万能验证码在生产环境**必须不存在**（f-10-01 R-001 的底线）。
	//
	// 这里选择报错而不是清空该值：清空会让服务照常起来，而部署者以为
	// 「我配置的那个码还能用」——直到某天发现它用不了，或者更糟，直到
	// 有人把它打开。报错则让这个问题在部署那一刻就暴露。
	if c.Env == EnvProduction && strings.TrimSpace(c.Security.DevBypassCode) != "" {
		return fmt.Errorf("生产环境不允许配置 SECURITY_DEV_BYPASS_CODE：它会让二次验证形同虚设，" +
			"任何知道该值的人都能直接通过高风险操作的门禁")
	}
	return nil
}

// String 返回脱敏后的配置摘要，避免密码与密钥进入日志。
func (c Config) String() string {
	return fmt.Sprintf(
		"env=%s debug=%t http=%s db.driver=%s db.name=%s db.maxOpen=%d session.idle=%s session.absolute=%s secureCookie=%t",
		c.Env, c.Debug, c.HTTP.Addr(), c.DB.Driver, c.DB.Name, c.DB.MaxOpenConns,
		c.Session.IdleTimeout, c.Session.AbsoluteTimeout, c.Session.SecureCookie,
	)
}

// Load 从环境变量读取配置并校验。
func Load() (Config, error) {
	appEnv := env("APP_ENV", EnvDevelopment)
	secret := env("SESSION_SECRET", "")

	// 本地开发未配置密钥时临时生成：重启即失效、多实例之间不通用，
	// 但足以让开发环境开箱可跑。生产环境由 Validate 强制要求配置。
	if secret == "" && appEnv != EnvProduction {
		secret = randomSecret()
		log.Printf("[config] 未配置 SESSION_SECRET，已生成临时密钥；重启后全部会话失效")
	}

	cfg := Config{
		Env:   appEnv,
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
		Session: Session{
			Secret:          secret,
			SecureCookie:    envBool("SESSION_SECURE_COOKIE", appEnv == EnvProduction),
			IdleTimeout:     envDuration("SESSION_IDLE_TIMEOUT", 2*time.Hour),
			AbsoluteTimeout: envDuration("SESSION_ABSOLUTE_TIMEOUT", 7*24*time.Hour),
		},
		Agent: Agent{
			Transport: env("AGENT_TRANSPORT", AgentTransportMock),
		},
		Security: Security{
			DevBypassCode: env("SECURITY_DEV_BYPASS_CODE", ""),
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

// randomSecret 生成一次性签名密钥，仅供本地开发使用。
//
// 返回空串表示随机源不可用——此时令牌签发会因密钥强度不足而失败，
// 这是刻意的：与其用可预测的密钥继续运行，不如让启动失败暴露问题。
func randomSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
