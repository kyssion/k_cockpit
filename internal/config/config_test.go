package config

import (
	"strings"
	"testing"
	"time"
)

func TestDB_DSN(t *testing.T) {
	tests := []struct {
		name    string
		db      DB
		want    string
		wantErr bool
	}{
		{
			name: "postgres 拼接完整连接串",
			db: DB{
				Driver: DriverPostgres, Host: "db.local", Port: 5432,
				User: "app", Password: "pwd", Name: "k_cockpit", SSLMode: "require",
			},
			want: "host=db.local port=5432 user=app password=pwd dbname=k_cockpit sslmode=require",
		},
		{
			name: "sqlite 直接使用文件路径",
			db:   DB{Driver: DriverSQLite, Path: "data/test.db"},
			want: "data/test.db",
		},
		{
			name:    "未知驱动返回错误",
			db:      DB{Driver: "mysql"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.db.DSN()
			if tt.wantErr {
				if err == nil {
					t.Fatal("期望返回错误，实际为 nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("非预期错误: %v", err)
			}
			if got != tt.want {
				t.Errorf("DSN() = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

func TestConfig_Validate(t *testing.T) {
	valid := Config{
		HTTP: HTTP{Host: "0.0.0.0", Port: 8080},
		DB:   DB{Driver: DriverSQLite, Path: "data/test.db", MaxOpenConns: 25, MaxIdleConns: 5},
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "合法配置",
			mutate: func(*Config) {},
		},
		{
			name:    "端口越界",
			mutate:  func(c *Config) { c.HTTP.Port = 70000 },
			wantErr: "APP_PORT",
		},
		{
			name:    "监听地址为空",
			mutate:  func(c *Config) { c.HTTP.Host = "  " },
			wantErr: "APP_HOST",
		},
		{
			name:    "sqlite 缺少路径",
			mutate:  func(c *Config) { c.DB.Path = "" },
			wantErr: "DB_PATH",
		},
		{
			name: "postgres 缺少驱动之外的约束",
			mutate: func(c *Config) {
				c.DB = DB{Driver: DriverPostgres, Host: "h", Port: 5432, Name: "n", MaxOpenConns: 0}
			},
			wantErr: "DB_MAX_OPEN_CONNS",
		},
		{
			name:    "空闲连接数大于最大连接数",
			mutate:  func(c *Config) { c.DB.MaxIdleConns = 100 },
			wantErr: "DB_MAX_IDLE_CONNS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)

			err := cfg.Validate()

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过校验，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望包含 %q 的错误，实际为 nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoad_FromEnv(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("APP_DEBUG", "true")
	t.Setenv("APP_HOST", "127.0.0.1")
	t.Setenv("APP_PORT", "9090")
	t.Setenv("DB_DRIVER", DriverPostgres)
	t.Setenv("DB_HOST", "db.local")
	t.Setenv("DB_PORT", "5433")
	t.Setenv("DB_USER", "app")
	t.Setenv("DB_PASSWORD", "pwd")
	t.Setenv("DB_NAME", "k_cockpit_test")
	t.Setenv("DB_SSLMODE", "require")
	t.Setenv("DB_MAX_OPEN_CONNS", "10")
	t.Setenv("DB_MAX_IDLE_CONNS", "2")
	t.Setenv("DB_CONN_MAX_LIFETIME", "30m")
	t.Setenv("DB_AUTO_MIGRATE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	if cfg.Env != "test" || !cfg.Debug {
		t.Errorf("应用配置读取有误: env=%q debug=%t", cfg.Env, cfg.Debug)
	}
	if cfg.HTTP.Addr() != "127.0.0.1:9090" {
		t.Errorf("HTTP 地址 = %q, 期望 %q", cfg.HTTP.Addr(), "127.0.0.1:9090")
	}
	if cfg.DB.Driver != DriverPostgres || cfg.DB.Name != "k_cockpit_test" {
		t.Errorf("数据库配置读取有误: %+v", cfg.DB)
	}
	if cfg.DB.MaxOpenConns != 10 || cfg.DB.MaxIdleConns != 2 {
		t.Errorf("连接池配置读取有误: open=%d idle=%d", cfg.DB.MaxOpenConns, cfg.DB.MaxIdleConns)
	}
	if cfg.DB.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("ConnMaxLifetime = %v, 期望 30m", cfg.DB.ConnMaxLifetime)
	}
	if !cfg.DB.AutoMigrate {
		t.Error("AutoMigrate 应为 true")
	}
}

func TestLoad_InvalidEnvFallsBackToDefault(t *testing.T) {
	// 非法数字不应导致启动失败，而应回退到默认值。
	t.Setenv("APP_PORT", "not-a-number")
	t.Setenv("DB_MAX_OPEN_CONNS", "abc")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.HTTP.Port != 8080 {
		t.Errorf("端口 = %d, 期望回退到默认值 8080", cfg.HTTP.Port)
	}
	if cfg.DB.MaxOpenConns != 25 {
		t.Errorf("MaxOpenConns = %d, 期望回退到默认值 25", cfg.DB.MaxOpenConns)
	}
}

func TestConfig_String_不泄漏密码(t *testing.T) {
	cfg := Config{
		Env:  "production",
		HTTP: HTTP{Host: "0.0.0.0", Port: 8080},
		DB: DB{
			Driver: DriverPostgres, Host: "db.local", Port: 5432,
			User: "app", Password: "super-secret", Name: "k_cockpit",
			MaxOpenConns: 25,
		},
	}

	if s := cfg.String(); strings.Contains(s, "super-secret") {
		t.Errorf("配置摘要不应包含密码: %s", s)
	}
}
