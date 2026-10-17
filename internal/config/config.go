// Package config owns validation, corruption recovery and atomic updates.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aplm123/android-ai-mcp/internal/atomicfile"
	"github.com/aplm123/android-ai-mcp/internal/buildinfo"
)

const SchemaVersion = 1

type Config struct {
	SchemaVersion int             `json:"schemaVersion"`
	Network       Network         `json:"network"`
	Paths         Paths           `json:"paths"`
	Limits        Limits          `json:"limits"`
	Security      Security        `json:"security"`
	Capabilities  map[string]bool `json:"capabilities"`
}

type Network struct {
	Port int `json:"port"`
	// ListenLAN 控制是否监听直接连接的局域网地址。
	//   false → 仅监听 127.0.0.1 / ::1（本机可达，最安全）
	//   true  → 额外监听 0.0.0.0 / ::（同网段可达，受 security 认证约束）
	//
	// 曾有一个 listenLoopback 字段，但它从未真正控制监听（环回始终监听），
	// 属于「配置欺骗」，已移除；环回不可关闭，否则会把自己锁在外面。
	ListenLAN      bool     `json:"listenLan"`
	LegacySSE      bool     `json:"legacySse"`
	AllowedOrigins []string `json:"allowedOrigins"`
}

type Paths struct {
	StateDir     string `json:"stateDir"`
	WorkDir      string `json:"workDir"`
	DownloadsDir string `json:"downloadsDir"`
	UploadsDir   string `json:"uploadsDir"`
	ArtifactsDir string `json:"artifactsDir"`
	TempDir      string `json:"tempDir"`
}

type Limits struct {
	MaxConnections int `json:"maxConnections"`
	// MaxStreams 是流式连接（MCP / SSE）的独立配额。
	// 与 maxConnections 分开，避免长期挂起的流把普通调用挤死。
	MaxStreams int `json:"maxStreams"`
	// StreamIdleTTLSeconds 是流连接的空闲判定阈值（秒）。
	StreamIdleTTLSeconds  int   `json:"streamIdleTtlSeconds"`
	MaxRequestBytes       int64 `json:"maxRequestBytes"`
	TotalTasks            int   `json:"totalTasks"`
	HeavyTasks            int   `json:"heavyTasks"`
	ShellTimeoutSeconds   int   `json:"shellTimeoutSeconds"`
	TransferChunkBytes    int64 `json:"transferChunkBytes"`
	TransferMaxBytes      int64 `json:"transferMaxBytes"`
	ResultPreviewBytes    int64 `json:"resultPreviewBytes"`
	ArtifactTTLSeconds    int   `json:"artifactTtlSeconds"`
	ShutdownGraceSeconds  int   `json:"shutdownGraceSeconds"`
	UploadIdleTTLSeconds  int   `json:"uploadIdleTtlSeconds"`
	DownloadRetryAttempts int   `json:"downloadRetryAttempts"`
}

type Security struct {
	// Anonymous 为兼容旧配置保留，但不再具有「拒绝认证」的含义：
	// 认证是否启用由 RequireToken / LoopbackAnonymous 决定。
	Anonymous       bool `json:"anonymous"`
	OnLinkOnly      bool `json:"onLinkOnly"`
	ValidateHost    bool `json:"validateHost"`
	ValidateOrigin  bool `json:"validateOrigin"`
	AllowCORS       bool `json:"allowCors"`
	DropFrontendUID int  `json:"dropFrontendUid"`

	// RequireToken 要求非环回来源携带 Bearer token。
	// 默认 true：局域网访问必须认证；环回是否免认证由 LoopbackAnonymous 决定。
	RequireToken bool `json:"requireToken"`
	// LoopbackAnonymous 允许环回来源免认证。
	// 默认 true，使本机调用（含 MCP 客户端）在升级后无需任何改动。
	LoopbackAnonymous bool `json:"loopbackAnonymous"`
	// TokenFile 是 bearer token 的存放路径；为空时使用 <stateDir>/token。
	TokenFile string `json:"tokenFile"`
}

type LoadReport struct {
	Created       bool   `json:"created"`
	Recovered     bool   `json:"recovered"`
	CorruptBackup string `json:"corruptBackup"`
	Warning       string `json:"warning"`
}

func Default() Config {
	work := buildinfo.DefaultWorkDir
	state := buildinfo.DefaultStateDir
	return Config{
		SchemaVersion: SchemaVersion,
		Network: Network{
			Port:           buildinfo.DefaultPort,
			ListenLAN:      true,
			LegacySSE:      true,
			AllowedOrigins: []string{},
		},
		Paths: Paths{
			StateDir:     state,
			WorkDir:      work,
			DownloadsDir: filepath.Join(work, "downloads"),
			UploadsDir:   filepath.Join(work, "uploads"),
			ArtifactsDir: filepath.Join(work, "output"),
			TempDir:      filepath.Join(state, "tmp"),
		},
		Limits: Limits{
			MaxConnections:        32,
			MaxStreams:            64,
			StreamIdleTTLSeconds:  600,
			MaxRequestBytes:       8 << 20,
			TotalTasks:            8,
			HeavyTasks:            2,
			ShellTimeoutSeconds:   120,
			TransferChunkBytes:    4 << 20,
			TransferMaxBytes:      0,
			ResultPreviewBytes:    64 << 10,
			ArtifactTTLSeconds:    3600,
			ShutdownGraceSeconds:  15,
			UploadIdleTTLSeconds:  86400,
			DownloadRetryAttempts: 4,
		},
		Security: Security{
			Anonymous:         true,
			OnLinkOnly:        true,
			ValidateHost:      true,
			ValidateOrigin:    true,
			AllowCORS:         false,
			DropFrontendUID:   2000,
			RequireToken:      true,
			LoopbackAnonymous: true,
		},
		Capabilities: map[string]bool{},
	}
}

func (c Config) Validate() error {
	var problems []string
	if c.SchemaVersion != SchemaVersion {
		problems = append(problems, fmt.Sprintf("schemaVersion 必须为 %d", SchemaVersion))
	}
	if c.Network.Port < 1 || c.Network.Port > 65535 {
		problems = append(problems, "network.port 必须在 1..65535")
	}
	for _, origin := range c.Network.AllowedOrigins {
		if origin == "*" {
			problems = append(problems, "不允许通配 Origin")
		}
	}
	requiredPaths := map[string]string{
		"paths.stateDir": c.Paths.StateDir,
		"paths.workDir":  c.Paths.WorkDir,
	}
	for name, value := range requiredPaths {
		if strings.TrimSpace(value) == "" || !portableAbs(value) {
			problems = append(problems, name+" 必须是绝对路径")
		}
	}
	if c.Limits.TotalTasks < 1 || c.Limits.TotalTasks > 128 {
		problems = append(problems, "limits.totalTasks 必须在 1..128")
	}
	if c.Limits.MaxStreams < 1 || c.Limits.MaxStreams > 1024 {
		problems = append(problems, "limits.maxStreams 必须在 1..1024")
	}
	if c.Limits.StreamIdleTTLSeconds < 30 || c.Limits.StreamIdleTTLSeconds > 86400 {
		problems = append(problems, "limits.streamIdleTtlSeconds 必须在 30..86400")
	}
	if c.Limits.HeavyTasks < 1 || c.Limits.HeavyTasks > c.Limits.TotalTasks {
		problems = append(problems, "limits.heavyTasks 必须在 1..totalTasks")
	}
	if c.Limits.ShellTimeoutSeconds < 1 || c.Limits.ShellTimeoutSeconds > 86400 {
		problems = append(problems, "limits.shellTimeoutSeconds 必须在 1..86400")
	}
	if c.Limits.TransferChunkBytes < 64<<10 || c.Limits.TransferChunkBytes > 64<<20 {
		problems = append(problems, "limits.transferChunkBytes 必须在 64 KiB..64 MiB")
	}
	if c.Limits.MaxRequestBytes < c.Limits.TransferChunkBytes {
		problems = append(problems, "limits.maxRequestBytes 不得小于传输块")
	}
	if c.Limits.ResultPreviewBytes < 4<<10 || c.Limits.ResultPreviewBytes > 1<<20 {
		problems = append(problems, "limits.resultPreviewBytes 必须在 4 KiB..1 MiB")
	}
	if c.Security.AllowCORS {
		problems = append(problems, "本版本禁止开放 CORS")
	}
	// 认证由 RequireToken / LoopbackAnonymous 决定。旧版用 anonymous 字段
	// 表达「强制匿名」；现在保留该字段只为兼容已有配置文件，不再据此拒绝。
	if !c.Security.OnLinkOnly || !c.Security.ValidateHost || !c.Security.ValidateOrigin {
		problems = append(problems, "onLinkOnly、validateHost、validateOrigin 必须保持启用")
	}
	// 若关闭环回免认证又关闭 token 要求，则本机调用也会被拒且无从认证，
	// 属于必然把自己锁死的组合，直接拒绝。
	if !c.Security.RequireToken && !c.Security.LoopbackAnonymous {
		problems = append(problems, "security.requireToken 与 loopbackAnonymous 不能同时为 false（会导致无法访问）")
	}
	if c.Security.DropFrontendUID != 2000 {
		problems = append(problems, "security.dropFrontendUid 必须为 Android shell UID 2000")
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func portableAbs(path string) bool {
	return filepath.IsAbs(path) || strings.HasPrefix(filepath.ToSlash(path), "/")
}

func Load(path string) (Config, LoadReport, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := Default()
		cfg.Paths.StateDir = filepath.Dir(path)
		derivePaths(&cfg)
		if err := Save(path, cfg); err != nil {
			return Config{}, LoadReport{}, err
		}
		return cfg, LoadReport{Created: true}, nil
	}
	if err != nil {
		return Config{}, LoadReport{}, fmt.Errorf("读取配置: %w", err)
	}
	cfg := Default()
	cfg.Paths.StateDir = filepath.Dir(path)
	cfg.Paths.DownloadsDir = ""
	cfg.Paths.UploadsDir = ""
	cfg.Paths.ArtifactsDir = ""
	cfg.Paths.TempDir = ""
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.Validate() != nil {
		reason := err
		if reason == nil {
			reason = cfg.Validate()
		}
		backup := path + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if renameErr := os.Rename(path, backup); renameErr != nil {
			return Config{}, LoadReport{}, fmt.Errorf("配置损坏且备份失败 (%v): %w", reason, renameErr)
		}
		cfg = Default()
		cfg.Paths.StateDir = filepath.Dir(path)
		derivePaths(&cfg)
		if saveErr := Save(path, cfg); saveErr != nil {
			return Config{}, LoadReport{}, fmt.Errorf("配置恢复保存失败: %w", saveErr)
		}
		return cfg, LoadReport{
			Recovered:     true,
			CorruptBackup: backup,
			Warning:       "配置损坏，已备份并恢复默认值: " + reason.Error(),
		}, nil
	}
	derivePaths(&cfg)
	return cfg, LoadReport{}, nil
}

func Save(path string, cfg Config) error {
	derivePaths(&cfg)
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("配置校验失败: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("编码配置: %w", err)
	}
	data = append(data, '\n')
	return atomicfile.Write(path, data, 0o600)
}

// Normalize returns a copy with every derived path and collection populated.
// Callers that keep a successfully saved Config in memory must use the same
// normalized representation that is written to disk.
func Normalize(cfg Config) Config {
	derivePaths(&cfg)
	return cfg
}

func EnsureDirectories(cfg Config) error {
	if err := EnsureStateDirectories(cfg); err != nil {
		return err
	}
	return EnsureWorkDirectories(cfg)
}

func EnsureStateDirectories(cfg Config) error {
	return ensureDirectories("内部状态", []string{
		cfg.Paths.StateDir,
		cfg.Paths.TempDir,
		filepath.Join(cfg.Paths.StateDir, "run"),
		filepath.Join(cfg.Paths.StateDir, "tasks"),
		filepath.Join(cfg.Paths.StateDir, "logs"),
		filepath.Join(cfg.Paths.StateDir, "versions"),
	})
}

func EnsureWorkDirectories(cfg Config) error {
	return ensureDirectories("用户工作", []string{
		cfg.Paths.WorkDir,
	})
}

func ensureDirectories(scope string, dirs []string) error {
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建%s目录 %s: %w", scope, dir, err)
		}
	}
	return nil
}

// ListenHost 返回应当用于监听的地址。
//
// 环回始终监听（服务自身与本机 MCP 客户端依赖它）。ListenLAN 为真时
// 额外监听全部地址，使同网段可直达——此时安全性由 accessPolicy 的
// 链路校验与 bearer token 认证共同保证。
func ListenHost(cfg Config) string {
	if cfg.Network.ListenLAN {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

func derivePaths(cfg *Config) {
	if cfg.Paths.DownloadsDir == "" {
		cfg.Paths.DownloadsDir = filepath.Join(cfg.Paths.WorkDir, "downloads")
	}
	if cfg.Paths.UploadsDir == "" {
		cfg.Paths.UploadsDir = filepath.Join(cfg.Paths.WorkDir, "uploads")
	}
	if cfg.Paths.ArtifactsDir == "" {
		cfg.Paths.ArtifactsDir = filepath.Join(cfg.Paths.WorkDir, "output")
	}
	if cfg.Paths.TempDir == "" {
		cfg.Paths.TempDir = filepath.Join(cfg.Paths.StateDir, "tmp")
	}
	if cfg.Capabilities == nil {
		cfg.Capabilities = map[string]bool{}
	}
}
