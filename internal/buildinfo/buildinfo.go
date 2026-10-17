package buildinfo

// 编译期可通过 -ldflags -X 覆盖的展示型常量（改名不需要改源码）。
// 注意 ModuleID 同时决定 /data/adb/modules/<id> 路径与用户数据目录，
// 它只在编译期可变；运行时逻辑一律引用 ModuleID，不硬编码字面量。
const (
	Name              = "aplm123 Root MCP"
	ModuleID          = "aplm123.root.mcp"
	DefaultVersion    = "0.09"
	ProtocolCurrent   = "2026-07-28"
	ProtocolPrevious  = "2025-11-25"
	ProtocolLegacySSE = "2024-11-05"
	DefaultPort       = 5322
	DefaultStateDir   = "/data/adb/aplm123-mcp"
	DefaultWorkDir    = "/storage/emulated/0/aplm123AI"
	// DefaultModuleDir 由 ModuleID 派生，避免两处不一致。
	DefaultModuleDir   = "/data/adb/modules/" + ModuleID
	DefaultSocketName  = "broker.sock"
	DefaultStableAfter = 300
)

var (
	Version          = DefaultVersion
	Commit           = "unknown"
	BuildTime        = "unknown"
	ModulePropSHA256 = ""

	// ModuleIDRuntime 允许通过 -X 覆盖模块 id（改名时使用）。
	// 为空时回退到编译期常量 ModuleID。
	ModuleIDRuntime = ""
)

// EffectiveModuleID 返回运行时生效的模块 id。
func EffectiveModuleID() string {
	if ModuleIDRuntime != "" {
		return ModuleIDRuntime
	}
	return ModuleID
}

// EffectiveModuleDir 返回运行时生效的模块安装目录。
func EffectiveModuleDir() string {
	return "/data/adb/modules/" + EffectiveModuleID()
}
