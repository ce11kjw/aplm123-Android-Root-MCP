package mcpapi

import "testing"

func TestResultTextModeSwitching(t *testing.T) {
	t.Cleanup(func() { SetResultTextMode("auto") })

	// auto + 没有请求上下文 → 走不上「最新协议」分支，必须是完整文本。
	SetResultTextMode("auto")
	if compactText(nil) {
		t.Fatal("auto 且无协议信息时应返回完整 JSON")
	}

	SetResultTextMode("on")
	if !compactText(nil) {
		t.Fatal("on 应精简")
	}

	SetResultTextMode("off")
	if compactText(nil) {
		t.Fatal("off 应返回完整 JSON")
	}
}

func TestResultTextModeFallsBackToAuto(t *testing.T) {
	t.Cleanup(func() { SetResultTextMode("auto") })
	for _, mode := range []string{"", "  ", "loud", "ON?"} {
		SetResultTextMode(mode)
		if got := ResultTextMode(); got != "auto" {
			t.Fatalf("%q 应回落为 auto，得到 %q", mode, got)
		}
	}
	// 大小写与空白应被接受。
	SetResultTextMode("  OFF ")
	if got := ResultTextMode(); got != "off" {
		t.Fatalf("大小写与空白应被收敛，得到 %q", got)
	}
}
