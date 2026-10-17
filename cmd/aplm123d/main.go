package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aplm123/android-ai-mcp/internal/atomicfile"
	"github.com/aplm123/android-ai-mcp/internal/broker"
	"github.com/aplm123/android-ai-mcp/internal/buildinfo"
	"github.com/aplm123/android-ai-mcp/internal/config"
	"github.com/aplm123/android-ai-mcp/internal/frontend"
	"github.com/aplm123/android-ai-mcp/internal/mcpapi"
	"github.com/aplm123/android-ai-mcp/internal/model"
	"github.com/aplm123/android-ai-mcp/internal/ops"
	"github.com/aplm123/android-ai-mcp/internal/schema"
	"github.com/aplm123/android-ai-mcp/internal/service"
	"github.com/aplm123/android-ai-mcp/internal/supervisor"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "supervisor":
		err = runSupervisor(os.Args[2:])
	case "broker":
		err = runBroker(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "webui-status":
		err = runWebUIStatus(os.Args[2:])
	case "start-diagnose":
		err = runStartDiagnose(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "config":
		err = runConfig(os.Args[2:])
	case "schema":
		err = runSchema(os.Args[2:])
	case "version", "--version", "-version":
		printVersion()
	case "help", "--help", "-h":
		usage()
	default:
		err = fmt.Errorf("未知子命令 %q", os.Args[1])
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "aplm123d:", err)
		os.Exit(1)
	}
}

func runSupervisor(arguments []string) error {
	flags := flag.NewFlagSet("supervisor", flag.ContinueOnError)
	stateDir := flags.String("state-dir", envOr("aplm123_STATE_DIR", buildinfo.DefaultStateDir), "内部状态目录")
	stableAfter := flags.Duration("stable-after", 5*time.Minute, "候选版本稳定标记时间")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	configPath := filepath.Join(*stateDir, "config.json")
	cfg, report, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if report.Warning != "" {
		_, _ = fmt.Fprintln(os.Stderr, report.Warning)
	}
	if err := config.EnsureStateDirectories(cfg); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	options := supervisor.DefaultOptions()
	options.StateDir = *stateDir
	options.StableAfter = *stableAfter
	options.Logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	go ensureWorkDirectories(ctx, cfg, options.Logger)
	go maintainMCPAddressFile(ctx, *stateDir, cfg.Network.Port, cfg.Network.ListenLAN, options.Logger)
	return supervisor.Run(ctx, options)
}

func runBroker(arguments []string) error {
	flags := flag.NewFlagSet("broker", flag.ContinueOnError)
	stateDir := flags.String("state-dir", envOr("aplm123_STATE_DIR", buildinfo.DefaultStateDir), "内部状态目录")
	socket := flags.String("socket", "", "Unix socket")
	pidFile := flags.String("frontend-pid-file", "", "frontend PID 文件")
	allowHost := flags.Bool("allow-unverified-host", runtime.GOOS != "android" && runtime.GOOS != "linux", "仅供非 Linux 主机测试")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if runtime.GOOS == "android" && effectiveUID() != 0 {
		return errors.New("broker 必须以 uid=0 运行")
	}
	if *socket == "" {
		*socket = filepath.Join(*stateDir, "run", buildinfo.DefaultSocketName)
	}
	if *pidFile == "" {
		*pidFile = filepath.Join(*stateDir, "run", "frontend.pid")
	}
	configPath := filepath.Join(*stateDir, "config.json")
	cfg, report, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if report.Warning != "" {
		_, _ = fmt.Fprintln(os.Stderr, report.Warning)
	}
	if err := config.EnsureStateDirectories(cfg); err != nil {
		return err
	}
	if err := config.EnsureWorkDirectories(cfg); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "共享存储尚未就绪，基础 MCP 服务继续启动:", err)
	}
	backend, err := service.New(configPath, cfg)
	if err != nil {
		return err
	}
	defer backend.Close()
	server, err := broker.NewServer(broker.ServerOptions{
		SocketPath:             *socket,
		FrontendPIDFile:        *pidFile,
		AllowedUIDs:            []uint32{0, 2000},
		SocketGID:              2000,
		MaxRequestBytes:        cfg.Limits.MaxRequestBytes,
		MaxConcurrent:          cfg.Limits.TotalTasks * 2,
		AllowUnverifiedForHost: *allowHost,
	}, backend)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go ensureWorkDirectories(ctx, cfg, slog.Default())
	return server.ListenAndServe(ctx)
}

func ensureWorkDirectories(ctx context.Context, cfg config.Config, logger *slog.Logger) {
	delay := time.Second
	reportedUnavailable := false
	for {
		err := config.EnsureWorkDirectories(cfg)
		if err == nil {
			if reportedUnavailable {
				logger.Info("共享存储已就绪，用户工作目录创建完成", "path", cfg.Paths.WorkDir)
			}
			return
		}
		if !reportedUnavailable {
			logger.Warn("共享存储尚未就绪；将后台重试且不阻塞 MCP 启动", "error", err)
			reportedUnavailable = true
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func runServe(arguments []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	stateDir := flags.String("state-dir", envOr("aplm123_STATE_DIR", buildinfo.DefaultStateDir), "内部状态目录")
	configPath := flags.String("config", "", "frontend 配置副本")
	socket := flags.String("socket", "", "broker Unix socket")
	degraded := flags.String("security-degraded", "", "安全降级原因")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *configPath == "" {
		*configPath = filepath.Join(*stateDir, "config.json")
	}
	if *socket == "" {
		*socket = filepath.Join(*stateDir, "run", buildinfo.DefaultSocketName)
	}
	cfg, report, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if report.Warning != "" {
		_, _ = fmt.Fprintln(os.Stderr, report.Warning)
	}
	rootClient := broker.Client{
		SocketPath:      *socket,
		DialTimeout:     5 * time.Second,
		MaxResponseSize: max(cfg.Limits.MaxRequestBytes, 16<<20),
	}
	api, err := mcpapi.New(rootClient)
	if err != nil {
		return err
	}
	taskExtension := mcpapi.TasksHandler{
		Client:       rootClient,
		Caller:       rootClient,
		MaxBodyBytes: cfg.Limits.MaxRequestBytes,
		PollInterval: 500 * time.Millisecond,
	}
	mcpHandlers := frontend.NewOfficialMCPHandlers(
		func(*http.Request) *mcp.Server {
			return api.Server()
		},
		frontend.OfficialMCPOptions{
			MaxRequestBodyBytes: cfg.Limits.MaxRequestBytes,
			SessionTimeout:      30 * time.Minute,
			Compatibility:       taskExtension,
		},
	)
	// 监听地址由配置统一决定：ListenLAN 为真才对外，否则仅环回。
	listenAddress := config.ListenHost(cfg)
	uploadMax := cfg.Limits.TransferMaxBytes
	if uploadMax <= 0 {
		uploadMax = math.MaxInt64
	}
	publicBroker := frontendBroker{client: rootClient, securityDegraded: *degraded}
	public, err := frontend.New(frontend.Options{
		Broker:             publicBroker,
		MCP:                mcpHandlers,
		Version:            buildinfo.Version,
		ProtocolCurrent:    buildinfo.ProtocolCurrent,
		ProtocolPrevious:   buildinfo.ProtocolPrevious,
		ProtocolLegacy:     buildinfo.ProtocolLegacySSE,
		ListenAddr:         listenAddress,
		Port:               cfg.Network.Port,
		WorkDir:            cfg.Paths.WorkDir,
		TransferDir:        filepath.Join(cfg.Paths.StateDir, "run", "transfer"),
		EnableLegacySSE:    cfg.Network.LegacySSE,
		AllowedOrigins:     cfg.Network.AllowedOrigins,
		RequireToken:       cfg.Security.RequireToken,
		LoopbackAnonymous:  cfg.Security.LoopbackAnonymous,
		TokenPath:          tokenPath(cfg),
		MaxRequestBytes:    cfg.Limits.MaxRequestBytes,
		MaxUploadBytes:     uploadMax,
		TransferChunkBytes: cfg.Limits.TransferChunkBytes,
		MaxConcurrent:      cfg.Limits.MaxConnections,
		MaxStreams:         cfg.Limits.MaxStreams,
		StreamIdleTimeout:  time.Duration(cfg.Limits.StreamIdleTTLSeconds) * time.Second,
		UploadTTL:          time.Duration(cfg.Limits.UploadIdleTTLSeconds) * time.Second,
		ArtifactTTL:        time.Duration(cfg.Limits.ArtifactTTLSeconds) * time.Second,
	})
	if err != nil {
		return err
	}
	// 面板的三个安全开关写回配置文件。监听范围变更需重启，
	// 认证开关由 frontend 自行即时应用。
	public.SetSecurityApplier(func(change frontend.SecurityChange) error {
		updated := cfg
		if change.ListenLAN != nil {
			updated.Network.ListenLAN = *change.ListenLAN
		}
		if change.RequireToken != nil {
			updated.Security.RequireToken = *change.RequireToken
		}
		if change.LoopbackAnonymous != nil {
			updated.Security.LoopbackAnonymous = *change.LoopbackAnonymous
		}
		if err := updated.Validate(); err != nil {
			return err
		}
		return config.Save(*configPath, updated)
	})
	api.SetPublisher(func(path, name, sha256 string, ttl time.Duration) (model.Artifact, error) {
		artifact, err := public.PublishFile(path, name, sha256, ttl)
		if err != nil {
			return model.Artifact{}, err
		}
		artifactHost := "127.0.0.1"
		if cfg.Network.ListenLAN {
			artifactHost = firstOnLinkAddress()
		}
		return model.Artifact{
			Name:      artifact.Name,
			Path:      artifact.Path,
			URI:       "http://" + net.JoinHostPort(artifactHost, strconv.Itoa(cfg.Network.Port)) + artifact.DownloadURL,
			MediaType: mime.TypeByExtension(filepath.Ext(artifact.Name)),
			Size:      artifact.Size,
			SHA256:    artifact.SHA256,
			ExpiresAt: artifact.ExpiresAt,
		}, nil
	})

	httpServer := &http.Server{
		Addr:              public.ListenAddress(),
		Handler:           public,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Limits.ShutdownGraceSeconds)*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	_, _ = fmt.Fprintf(os.Stderr, "aplm123 MCP frontend 监听 http://%s（匿名 Root LAN=%v）\n", public.ListenAddress(), cfg.Network.ListenLAN)
	err = httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func runStatus(arguments []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	stateDir := flags.String("state-dir", envOr("aplm123_STATE_DIR", buildinfo.DefaultStateDir), "内部状态目录")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	cfg, _, err := config.Load(filepath.Join(*stateDir, "config.json"))
	if err != nil {
		return err
	}
	url := "http://127.0.0.1:" + strconv.Itoa(cfg.Network.Port) + "/status"
	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("服务未就绪: %w", err)
	}
	defer response.Body.Close()
	var value any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		return err
	}
	return writePrettyJSON(os.Stdout, value)
}

func runDoctor(arguments []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	stateDir := flags.String("state-dir", envOr("aplm123_STATE_DIR", buildinfo.DefaultStateDir), "内部状态目录")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	configPath := filepath.Join(*stateDir, "config.json")
	cfg, report, err := config.Load(configPath)
	if err != nil {
		return err
	}
	checks := map[string]any{
		"config": map[string]any{
			"valid":     cfg.Validate() == nil,
			"recovered": report.Recovered,
			"warning":   report.Warning,
		},
		"runtime": map[string]any{
			"goos":   runtime.GOOS,
			"goarch": runtime.GOARCH,
			"uid":    effectiveUID(),
			"root":   effectiveUID() == 0,
		},
	}
	manager := ops.New(ops.Config{
		WorkDir:      cfg.Paths.WorkDir,
		StateDir:     cfg.Paths.StateDir,
		ShellTimeout: 30 * time.Second,
	})
	defer manager.Close()
	capability := manager.Execute(context.Background(), ops.Request{Tool: "aplm_capabilities", Args: map[string]any{"action": "probe"}})
	checks["capabilities"] = capability
	return writePrettyJSON(os.Stdout, map[string]any{
		"checks": checks,
		"status": map[string]any{
			"ok":   capability.Success,
			"code": capability.Code,
			"uid":  effectiveUID(),
		},
	})
}

// runWebUIStatus 为 KernelSU/APatch 的 WebUI 提供一份**不依赖 HTTP 服务**的状态快照。
//
// WebView 里无法保证能访问 127.0.0.1:5322（服务未起、端口被占、或管理器沙箱限制），
// 因此这里直接读配置与运行目录，尽量把关键信息给出来；
// 服务在线时再顺带附上实时状态。
func runWebUIStatus(arguments []string) error {
	flags := flag.NewFlagSet("webui-status", flag.ContinueOnError)
	stateDir := flags.String("state-dir", envOr("aplm123_STATE_DIR", buildinfo.DefaultStateDir), "内部状态目录")
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	snapshot := map[string]any{
		"moduleId": buildinfo.EffectiveModuleID(),
		"name":     buildinfo.Name,
		"version":  buildinfo.Version,
		"stateDir": *stateDir,
	}

	cfg, _, err := config.Load(filepath.Join(*stateDir, "config.json"))
	if err == nil {
		snapshot["port"] = cfg.Network.Port
		snapshot["listenLan"] = cfg.Network.ListenLAN
		snapshot["anonymous"] = cfg.Security.Anonymous
		snapshot["workDir"] = cfg.Paths.WorkDir
		snapshot["maxConnections"] = cfg.Limits.MaxConnections
		snapshot["maxStreams"] = cfg.Limits.MaxStreams
		snapshot["streamIdleTtlSeconds"] = cfg.Limits.StreamIdleTTLSeconds
	} else {
		snapshot["configError"] = err.Error()
	}

	// supervisor 存活情况。
	//
	// 注意：WebUI 的命令桥可能以非 root 身份执行本命令，
	// 此时对 root 进程做 kill(0) 会返回 EPERM，单凭信号探测会误判为"未运行"。
	// 因此这里采用多来源交叉验证，任一来源确认为活着即算运行：
	//   1) PID 文件 + 信号探测成功（root 场景，最直接）
	//   2) /proc/<pid> 存在（不依赖权限）
	//   3) 本地 HTTP 健康检查成功（进程活着且已监听）
	pidFile := filepath.Join(*stateDir, "run", "supervisor.pid")
	pid := ""
	if raw, readErr := os.ReadFile(pidFile); readErr == nil {
		pid = strings.TrimSpace(string(raw))
	}
	running := false
	aliveReason := ""
	if pid != "" {
		pidNum := atoiSafe(pid)
		if pidNum > 0 {
			// 来源 2：/proc/<pid> 存在即视为存活（无需任何权限）
			if _, statErr := os.Stat(filepath.Join("/proc", pid)); statErr == nil {
				running = true
				aliveReason = "proc"
			}
			// 来源 1：信号探测（root 时可用，且能识别僵尸进程）
			if process, findErr := os.FindProcess(pidNum); findErr == nil {
				if signalErr := process.Signal(syscall.Signal(0)); signalErr == nil {
					running = true
					aliveReason = "signal"
				}
			}
		}
	}
	snapshot["supervisorPid"] = pid
	snapshot["running"] = running
	if aliveReason != "" {
		snapshot["aliveSource"] = aliveReason
	}

	// 手动停止 / 崩溃保护标记，便于面板解释「为什么起不来」。
	if _, statErr := os.Stat(filepath.Join(*stateDir, "manual-stop")); statErr == nil {
		snapshot["manuallyStopped"] = true
	}
	if _, statErr := os.Stat(filepath.Join(*stateDir, "crash-loop.blocked")); statErr == nil {
		snapshot["crashLoopBlocked"] = true
	}

	// 来源 3：本地 HTTP 健康检查。
	// 它同时是"服务确实可用"的最强证据（进程活着且已监听），
	// 因此在 PID 文件缺失或权限不足时用它兜底。
	httpHealthy := false
	if err == nil {
		healthURL := "http://127.0.0.1:" + strconv.Itoa(cfg.Network.Port) + "/health"
		client := http.Client{Timeout: 2 * time.Second}
		if response, getErr := client.Get(healthURL); getErr == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				httpHealthy = true
			}
		}
	}
	if httpHealthy && !running {
		running = true
		snapshot["running"] = true
		snapshot["aliveSource"] = "http"
		snapshot["pidFileStale"] = true
	}

	// 服务在线时补上实时连接池水位与任务数。
	if err == nil && running {
		url := "http://127.0.0.1:" + strconv.Itoa(cfg.Network.Port) + "/status"
		client := http.Client{Timeout: 2 * time.Second}
		if response, getErr := client.Get(url); getErr == nil {
			defer response.Body.Close()
			var live map[string]any
			if decodeErr := json.NewDecoder(response.Body).Decode(&live); decodeErr == nil {
				snapshot["service"] = "running"
				if value, ok := live["connections"]; ok {
					snapshot["connections"] = value
				}
				if value, ok := live["uptimeSeconds"]; ok {
					snapshot["uptimeSeconds"] = value
				}
				if value, ok := live["taskCount"]; ok {
					snapshot["taskCount"] = value
				}
				if value, ok := live["rootFramework"]; ok {
					snapshot["rootFramework"] = value
				}
				if value, ok := live["mcpAddress"]; ok {
					snapshot["mcpAddress"] = value
				}
			}
		} else {
			snapshot["service"] = "stopped"
			snapshot["httpError"] = getErr.Error()
		}
	}
	return writePrettyJSON(os.Stdout, snapshot)
}

func atoiSafe(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

// runStartDiagnose 逐段检查启动链路，并把结论与原始证据一起打印出来。
//
// 存在的原因：service.sh 由 root 管理器在开机时调用，输出通常被丢弃；
// 守护进程若在写入日志前就退出（例如早期的哈希校验失败），
// 用户会看到"启动不了但没有任何日志"。本命令把每一环的状态、原始错误
// 与修复建议一次性给出，无需再逐处手工排查。
func runStartDiagnose(arguments []string) error {
	flags := flag.NewFlagSet("start-diagnose", flag.ContinueOnError)
	stateDir := flags.String("state-dir", envOr("aplm123_STATE_DIR", buildinfo.DefaultStateDir), "内部状态目录")
	moduleDir := flags.String("module-dir", envOr("aplm123_MODULE_DIR", buildinfo.DefaultModuleDir), "模块安装目录")
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	type step struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail,omitempty"`
		Hint   string `json:"hint,omitempty"`
	}
	steps := make([]step, 0, 12)
	add := func(name string, ok bool, detail, hint string) {
		steps = append(steps, step{Name: name, OK: ok, Detail: detail, Hint: hint})
	}

	// 1) 运行环境
	add("runtime",
		true,
		fmt.Sprintf("goos=%s goarch=%s uid=%d moduleId=%s version=%s",
			runtime.GOOS, runtime.GOARCH, effectiveUID(), buildinfo.EffectiveModuleID(), buildinfo.Version),
		"")

	// 2) 模块目录与 module.prop
	propPath := filepath.Join(*moduleDir, "module.prop")
	if raw, propErr := os.ReadFile(propPath); propErr == nil {
		firstLine := strings.SplitN(string(raw), "\n", 2)[0]
		add("module.prop", true, fmt.Sprintf("%s (%s)", propPath, strings.TrimSpace(firstLine)), "")
	} else {
		add("module.prop", false, propErr.Error(),
			"模块未正确安装或路径不对；用 --module-dir 指定实际位置")
	}

	// 3) 守护进程二进制
	daemon := filepath.Join(*moduleDir, "bin", "aplm123d")
	if info, statErr := os.Stat(daemon); statErr == nil {
		mode := info.Mode()
		executable := mode.IsRegular() && mode.Perm()&0o111 != 0
		detail := fmt.Sprintf("%s size=%d mode=%s", daemon, info.Size(), mode.Perm())
		hint := ""
		if !executable {
			hint = "二进制不可执行，请重新安装模块（安装脚本会设置 0755）"
		}
		add("daemon-binary", executable, detail, hint)
		if header, readErr := readFileHeader(daemon, 20); readErr == nil {
			// 非 Android 环境下本机架构不同，这里只报告魔数，不做判定
			add("daemon-elf", true, fmt.Sprintf("header=% x", header), "")
		}
	} else {
		add("daemon-binary", false, statErr.Error(),
			"模块 bin/ 目录不完整；请卸载后重新安装")
	}

	// 4) 状态目录可写性
	if mkErr := os.MkdirAll(*stateDir, 0o710); mkErr == nil {
		probe := filepath.Join(*stateDir, ".write-probe")
		if writeErr := os.WriteFile(probe, []byte("ok"), 0o600); writeErr == nil {
			_ = os.Remove(probe)
			add("state-dir", true, *stateDir+" 可写", "")
		} else {
			add("state-dir", false, writeErr.Error(), "检查 /data/adb 权限与 SELinux 上下文")
		}
	} else {
		add("state-dir", false, mkErr.Error(), "无法创建状态目录，检查 /data/adb 是否可写")
	}

	// 5) 阻断标记
	for _, marker := range []struct{ file, hint string }{
		{"manual-stop", "存在 manual-stop：服务被手动停止，启动会被跳过。删除该文件或用 force 模式启动"},
		{"crash-loop.blocked", "存在 crash-loop.blocked：因连续崩溃被安全暂停。删除该文件后重试"},
	} {
		path := filepath.Join(*stateDir, marker.file)
		if _, statErr := os.Stat(path); statErr == nil {
			add("blocker:"+marker.file, false, path+" 存在", marker.hint)
		} else {
			add("blocker:"+marker.file, true, "无 "+marker.file+" 标记", "")
		}
	}

	// 6) 启动锁残留
	lockDir := filepath.Join(*stateDir, "run", "start.lock")
	if entries, readErr := os.ReadDir(lockDir); readErr == nil {
		owner := ""
		if raw, ownerErr := os.ReadFile(filepath.Join(lockDir, "owner")); ownerErr == nil {
			owner = strings.TrimSpace(string(raw))
		}
		add("start-lock", false,
			fmt.Sprintf("%s 存在，条目=%d owner=%q", lockDir, len(entries), owner),
			"残留的启动锁会阻止再次启动。确认无 supervisor 在跑后删除该目录")
	} else {
		add("start-lock", true, "无残留启动锁", "")
	}

	// 7) PID 文件与服务存活
	pidPath := filepath.Join(*stateDir, "run", "supervisor.pid")
	if raw, readErr := os.ReadFile(pidPath); readErr == nil {
		pid := strings.TrimSpace(string(raw))
		alive := false
		if process, findErr := os.FindProcess(atoiSafe(pid)); findErr == nil {
			alive = process.Signal(syscall.Signal(0)) == nil
		}
		detail := fmt.Sprintf("PID=%s 存活=%t", pid, alive)
		hint := ""
		if !alive {
			hint = "PID 文件指向的进程已不存在（陈旧文件），删除后重试启动"
		}
		add("supervisor-pid", alive, detail, hint)
	} else {
		add("supervisor-pid", true, "无 PID 文件（服务当前未运行）", "")
	}

	// 8) 配置
	if cfg, _, cfgErr := config.Load(filepath.Join(*stateDir, "config.json")); cfgErr == nil {
		add("config", true,
			fmt.Sprintf("port=%d listenLAN=%t workDir=%s", cfg.Network.Port, cfg.Network.ListenLAN, cfg.Paths.WorkDir),
			"")
		if validateErr := cfg.Validate(); validateErr != nil {
			add("config-validate", false, validateErr.Error(), "运行 aplm123d config reset 恢复默认配置")
		} else {
			add("config-validate", true, "配置校验通过", "")
		}
	} else {
		add("config", false, cfgErr.Error(), "配置损坏时会自动回退默认值；仍报错则删除该文件")
	}

	// 9) 读取启动日志尾部，作为原始证据
	bootLog := filepath.Join(*stateDir, "logs", "boot.log")
	bootTail := ""
	if raw, readErr := os.ReadFile(bootLog); readErr == nil {
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		bootTail = strings.Join(lines, "\n")
	}
	supervisorLog := filepath.Join(*stateDir, "logs", "supervisor.log")
	supervisorTail := ""
	if raw, readErr := os.ReadFile(supervisorLog); readErr == nil {
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		supervisorTail = strings.Join(lines, "\n")
	}

	// 汇总：只有阻断性问题才算不通过
	blocking := 0
	for _, item := range steps {
		if !item.OK && (strings.HasPrefix(item.Name, "blocker:") ||
			item.Name == "daemon-binary" || item.Name == "state-dir" || item.Name == "start-lock") {
			blocking++
		}
	}

	report := map[string]any{
		"ready":          blocking == 0,
		"blockingIssues": blocking,
		"steps":          steps,
		"logs": map[string]any{
			"bootLogPath":       bootLog,
			"bootLogTail":       bootTail,
			"supervisorLogPath": supervisorLog,
			"supervisorLogTail": supervisorTail,
		},
	}
	if blocking > 0 {
		report["conclusion"] = "存在阻断启动的问题，请按上面各步的 hint 处理"
	} else if strings.Contains(supervisorTail, "拒绝运行服务") ||
		strings.Contains(supervisorTail, "签名哈希") {
		// 识别历史版本的自我完整性校验失败（0.04 起已移除该机制）。
		// 命中说明设备上装的是旧二进制，或日志是升级前留下的。
		report["conclusion"] = "启动日志显示守护进程曾被自我完整性校验拒绝。" +
			"该机制自 0.04 起已移除；若仍出现，说明设备上运行的仍是旧版二进制，" +
			"请卸载模块后重新安装当前版本"
	} else if bootTail == "" && supervisorTail == "" {
		report["conclusion"] = "没有任何启动日志。若刚安装，请重启设备；" +
			"若已重启仍无日志，说明 service.sh 未被 root 管理器执行，" +
			"请检查模块是否被禁用（disable 文件）或管理器未启用开机脚本"
	} else {
		report["conclusion"] = "未发现阻断项；若仍无法启动，请查看 logs 中的原始输出"
	}
	return writePrettyJSON(os.Stdout, report)
}

// readFileHeader 读取文件开头若干字节，用于识别 ELF 魔数。
func readFileHeader(path string, n int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buf := make([]byte, n)
	count, err := io.ReadFull(file, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:count], nil
}

func runConfig(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("config 需要 get、validate 或 reset")
	}
	action := arguments[0]
	flags := flag.NewFlagSet("config "+action, flag.ContinueOnError)
	stateDir := flags.String("state-dir", envOr("aplm123_STATE_DIR", buildinfo.DefaultStateDir), "内部状态目录")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	path := filepath.Join(*stateDir, "config.json")
	switch action {
	case "get":
		cfg, report, err := config.Load(path)
		if err != nil {
			return err
		}
		return writePrettyJSON(os.Stdout, map[string]any{"config": cfg, "loadReport": report})
	case "validate":
		cfg, _, err := config.Load(path)
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		return writePrettyJSON(os.Stdout, map[string]any{"success": true, "message": "配置有效"})
	case "reset":
		current, _, err := config.Load(path)
		if err != nil {
			return err
		}
		cfg := config.Default()
		cfg.Paths.StateDir = current.Paths.StateDir
		cfg.Paths.WorkDir = current.Paths.WorkDir
		cfg.Paths.DownloadsDir = ""
		cfg.Paths.UploadsDir = ""
		cfg.Paths.ArtifactsDir = ""
		cfg.Paths.TempDir = ""
		cfg = config.Normalize(cfg)
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		return writePrettyJSON(os.Stdout, map[string]any{"success": true, "message": "配置已恢复默认值", "config": cfg})
	default:
		return fmt.Errorf("未知 config 操作 %q", action)
	}
}

func runSchema(arguments []string) error {
	flags := flag.NewFlagSet("schema", flag.ContinueOnError)
	output := flags.String("output", "-", "输出文件；- 表示 stdout")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	data, err := schema.Marshal(buildinfo.ProtocolCurrent)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if *output == "-" {
		_, err = os.Stdout.Write(data)
		return err
	}
	return atomicfile.Write(*output, data, 0o644)
}

func printVersion() {
	_ = writePrettyJSON(os.Stdout, map[string]any{
		"name":      buildinfo.Name,
		"moduleId":  buildinfo.ModuleID,
		"version":   buildinfo.Version,
		"commit":    buildinfo.Commit,
		"buildTime": buildinfo.BuildTime,
		// 读出来才能让链接期 -X 注入的值留在二进制里：
		// 未被引用的变量会被链接器消除，而模块包校验会检查这个指纹是否存在。
		"modulePropSha256": buildinfo.ModulePropSHA256,
		"protocols":        []string{buildinfo.ProtocolCurrent, buildinfo.ProtocolPrevious, buildinfo.ProtocolLegacySSE},
	})
}

func usage() {
	_, _ = fmt.Fprintln(os.Stderr, `aplm123 Root MCP

用法:
  aplm123d supervisor [--state-dir DIR]
  aplm123d broker --state-dir DIR --socket PATH --frontend-pid-file PATH
  aplm123d serve --state-dir DIR --config PATH --socket PATH
  aplm123d status [--state-dir DIR]
  aplm123d webui-status [--state-dir DIR]
  aplm123d start-diagnose [--state-dir DIR] [--module-dir DIR]
  aplm123d doctor [--state-dir DIR]
  aplm123d config get|validate|reset [--state-dir DIR]
  aplm123d schema [--output schemas/tools.json]
  aplm123d version`)
}

type frontendBroker struct {
	client           broker.Client
	securityDegraded string
}

func (b frontendBroker) Call(ctx context.Context, tool string, args map[string]any) (frontend.Result, error) {
	result := b.client.Call(ctx, tool, args)
	return frontendResult(result), nil
}

func (b frontendBroker) Status(ctx context.Context) (map[string]any, error) {
	status, err := b.client.Status(ctx)
	if err != nil {
		return nil, err
	}
	if b.securityDegraded == "" {
		status["securityDegraded"] = false
	} else {
		status["securityDegraded"] = true
		status["securityDegradationReason"] = b.securityDegraded
	}
	return status, nil
}

func frontendResult(source model.Result) frontend.Result {
	artifacts := make([]any, 0, len(source.Artifacts))
	for _, artifact := range source.Artifacts {
		artifacts = append(artifacts, artifact)
	}
	return frontend.Result{
		Success:        source.Success,
		Code:           source.Code,
		Message:        source.Message,
		Data:           source.Data,
		Error:          source.Error,
		Stdout:         source.Stdout,
		Stderr:         source.Stderr,
		ExitCode:       source.ExitCode,
		DurationMS:     source.DurationMS,
		TaskID:         source.TaskID,
		RebootRequired: source.RebootRequired,
		Artifacts:      artifacts,
		Strategy:       source.Strategy,
	}
}

func writePrettyJSON(file *os.File, value any) error {
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// tokenPath 决定 bearer token 的存放位置。
// 显式配置优先；否则落在状态目录下，与其它运行时文件同处。
func tokenPath(cfg config.Config) string {
	if p := strings.TrimSpace(cfg.Security.TokenFile); p != "" {
		return p
	}
	return filepath.Join(cfg.Paths.StateDir, "token")
}

func maintainMCPAddressFile(
	ctx context.Context,
	stateDir string,
	port int,
	listenLAN bool,
	logger *slog.Logger,
) {
	path := filepath.Join(stateDir, "MCP地址.txt")
	lastContent := ""
	lastError := ""
	update := func() {
		address := ""
		if listenLAN {
			if detected, ok := firstLANAddress(); ok {
				address = detected
			}
		}
		value := mcpAddressFileContent(port, listenLAN, address)
		if value == lastContent {
			return
		}
		if err := atomicfile.Write(path, []byte(value), 0o644); err != nil {
			if message := err.Error(); message != lastError {
				logger.Warn("更新 MCP 地址文件失败", "path", path, "error", err)
				lastError = message
			}
			return
		}
		lastContent = value
		lastError = ""
	}
	update()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			update()
		}
	}
}

func mcpAddressFileContent(port int, listenLAN bool, address string) string {
	var content strings.Builder
	_, _ = fmt.Fprintf(&content, "本机地址：http://127.0.0.1:%d/mcp\n", port)
	if listenLAN && address != "" {
		_, _ = fmt.Fprintf(&content, "局域网地址：http://%s:%d/mcp\n", address, port)
	}
	return content.String()
}

func firstOnLinkAddress() string {
	if address, ok := firstLANAddress(); ok {
		return address
	}
	return "127.0.0.1"
}

func firstLANAddress() (string, bool) {
	interfaces, err := net.Interfaces()
	if err == nil {
		for preferredOnly := true; ; preferredOnly = false {
			for _, networkInterface := range interfaces {
				if networkInterface.Flags&net.FlagUp == 0 ||
					networkInterface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 ||
					isCellularNetworkInterface(networkInterface.Name) ||
					preferredOnly && !isPreferredLANInterface(networkInterface.Name) {
					continue
				}
				addresses, _ := networkInterface.Addrs()
				for _, address := range addresses {
					var ip net.IP
					switch value := address.(type) {
					case *net.IPNet:
						ip = value.IP
					case *net.IPAddr:
						ip = value.IP
					}
					if ip == nil || ip.IsLoopback() || ip.To4() == nil {
						continue
					}
					if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
						return ip.String(), true
					}
				}
			}
			if !preferredOnly {
				break
			}
		}
	}
	return "", false
}

func isCellularNetworkInterface(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{
		"rmnet", "ccmni", "pdp", "wwan", "cellular", "v4-rmnet", "r_rmnet",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func isPreferredLANInterface(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{
		"wlan", "wifi", "ap", "swlan", "eth", "rndis", "usb", "bt-pan",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
