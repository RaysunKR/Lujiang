// Package config 加载并校验服务端 YAML 配置。
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 是服务端配置。
type Config struct {
	Listen       string        `yaml:"listen"`        // ":7443"
	CertFile     string        `yaml:"cert_file"`     // 可选；与 key_file 同时为空且 auto_tls=true 时自签
	KeyFile      string        `yaml:"key_file"`
	AutoTLS      bool          `yaml:"auto_tls"`      // 缺证书时自签
	WebUsers     []WebUser     `yaml:"web_users"`     // Web 登录账号
	Clients      []ClientEntry `yaml:"clients"`       // 合法客户端 token 白名单
	SessionSecret string       `yaml:"session_secret"` // HMAC session cookie 密钥；为空时随机生成
}

// WebUser 是一个 Web 登录账号。PasswordHash 为 bcrypt 散列。
type WebUser struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

// ClientEntry 是一个被允许接入的客户端。Token 由客户端配置预填，服务端做相等校验。
type ClientEntry struct {
	ID    string `yaml:"id"`
	Token string `yaml:"token"`
}

// Load 从 path 读取并校验配置。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Listen == "" {
		c.Listen = ":7443"
	}
	if len(c.WebUsers) == 0 {
		return fmt.Errorf("config: at least one web_user is required")
	}
	for i, u := range c.WebUsers {
		if u.Username == "" || u.PasswordHash == "" {
			return fmt.Errorf("config: web_users[%d] requires username and password_hash", i)
		}
	}
	if len(c.Clients) == 0 {
		return fmt.Errorf("config: at least one client is required")
	}
	seen := map[string]bool{}
	for i, cl := range c.Clients {
		if cl.ID == "" || cl.Token == "" {
			return fmt.Errorf("config: clients[%d] requires id and token", i)
		}
		if seen[cl.ID] {
			return fmt.Errorf("config: client id %q duplicated", cl.ID)
		}
		seen[cl.ID] = true
	}
	return nil
}

// TokenForClient 返回给定 clientID 的预共享 token。
func (c *Config) TokenForClient(id string) (string, bool) {
	for _, cl := range c.Clients {
		if cl.ID == id {
			return cl.Token, true
		}
	}
	return "", false
}

// IDForToken 反查 token 所属 clientID。
func (c *Config) IDForToken(token string) (string, bool) {
	for _, cl := range c.Clients {
		if cl.Token == token {
			return cl.ID, true
		}
	}
	return "", false
}
