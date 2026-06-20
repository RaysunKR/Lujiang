// Package config 加载并校验客户端 YAML 配置。
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 是客户端配置。
type Config struct {
	ID         string `yaml:"id"`          // 必须与服务端 clients[].id 对应
	Token      string `yaml:"token"`       // 必须与服务端 clients[].token 对应
	ServerURL  string `yaml:"server_url"`  // 如 "wss://127.0.0.1:7443/api/tunnel"
	WorkspaceRoot string `yaml:"workspace_root"` // 项目默认根
	DataDir    string `yaml:"data_dir"`    // SQLite 等本地状态存放目录
	TLSSkipVerify bool `yaml:"tls_skip_verify"` // 自签证书场景跳过校验（仅开发）
}

// Load 从 path 读取并校验。
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
	if c.ID == "" {
		return fmt.Errorf("config: id is required")
	}
	if c.Token == "" {
		return fmt.Errorf("config: token is required")
	}
	if c.ServerURL == "" {
		return fmt.Errorf("config: server_url is required")
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.WorkspaceRoot == "" {
		c.WorkspaceRoot = "."
	}
	return nil
}
