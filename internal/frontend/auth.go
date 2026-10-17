package frontend

// Bearer token 认证。
//
// 设计目标：在不破坏本机用法的前提下，让「同网段可见」的暴露面具备身份校验。
//   环回 + LoopbackAnonymous  → 免认证（默认，升级零影响）
//   非环回 + RequireToken     → 必须携带正确 token
//
// token 用文件保存而不是每次随机：进程重启后客户端无需重新配置。

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// tokenBytes 是 token 的熵。32 字节 = 256 位，远超暴力枚举现实可能。
	tokenBytes = 32
	// tokenFileMode 保证只有 root 可读写；模块进程本身以 root 运行。
	tokenFileMode = 0o600
	// tokenCacheTTL 控制多久重新读一次文件，兼顾轮换及时性与 IO 开销。
	tokenCacheTTL = 2 * time.Second
)

var errTokenMissing = errors.New("尚未生成 bearer token")

// tokenStore 负责 token 的生成、读取与轮换。
type tokenStore struct {
	path string

	mu      sync.Mutex
	cached  []byte
	loadedAt time.Time
}

func newTokenStore(path string) (*tokenStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("token 路径为空")
	}
	return &tokenStore{path: path}, nil
}

// ensure 保证 token 文件存在，必要时生成。返回当前 token。
func (s *tokenStore) ensure() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureLocked()
}

func (s *tokenStore) ensureLocked() (string, error) {
	raw, err := os.ReadFile(s.path)
	if err == nil {
		token := strings.TrimSpace(string(raw))
		if token == "" {
			// 空文件说明上次生成中途失败，重新生成。
			return s.generateLocked()
		}
		s.cached = []byte(token)
		s.loadedAt = time.Now()
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("读取 token 失败: %w", err)
	}
	return s.generateLocked()
}

func (s *tokenStore) generateLocked() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机 token 失败: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return "", fmt.Errorf("创建 token 目录失败: %w", err)
	}
	// 先写临时文件再改名：避免并发读到半个 token。
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), tokenFileMode); err != nil {
		return "", fmt.Errorf("写入 token 失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("提交 token 失败: %w", err)
	}
	s.cached = []byte(token)
	s.loadedAt = time.Now()
	return token, nil
}

// rotate 生成新 token 并落盘，返回新值。旧 token 立即失效。
func (s *tokenStore) rotate() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generateLocked()
}

// current 返回当前 token；必要时读盘。缓存避免每个请求都做 IO。
func (s *tokenStore) current() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cached) > 0 && time.Since(s.loadedAt) < tokenCacheTTL {
		return s.cached, nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errTokenMissing
		}
		return nil, err
	}
	token := []byte(strings.TrimSpace(string(raw)))
	if len(token) == 0 {
		return nil, errTokenMissing
	}
	s.cached = token
	s.loadedAt = time.Now()
	return token, nil
}

// prefix 返回 token 前若干位，用于界面上核对而无需暴露完整值。
func (s *tokenStore) prefix(n int) string {
	token, err := s.current()
	if err != nil || len(token) < n {
		return ""
	}
	return string(token[:n])
}

// verify 以常量时间比较，避免通过响应时间推测 token。
func (s *tokenStore) verify(presented string) bool {
	expected, err := s.current()
	if err != nil || len(expected) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(expected, []byte(presented)) == 1
}

// bearerFromRequest 提取 Authorization: Bearer <token>。
func bearerFromRequest(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
