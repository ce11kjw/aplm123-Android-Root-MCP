package config

import "testing"

func TestCompactResultTextDefaultsToFull(t *testing.T) {
	cfg := Normalize(Default())
	if cfg.Limits.CompactResultText != ResultTextOff {
		t.Fatalf("默认应为 %q（完整结果），得到 %q", ResultTextOff, cfg.Limits.CompactResultText)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置应通过校验: %v", err)
	}
}

func TestCompactResultTextAcceptsKnownValues(t *testing.T) {
	for _, mode := range []string{ResultTextAuto, ResultTextOn, ResultTextOff} {
		cfg := Normalize(Default())
		cfg.Limits.CompactResultText = mode
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s 应通过校验: %v", mode, err)
		}
	}
}

func TestCompactResultTextRejectsUnknownValue(t *testing.T) {
	cfg := Normalize(Default())
	cfg.Limits.CompactResultText = "loud"
	if err := cfg.Validate(); err == nil {
		t.Fatal("未知取值应被拒绝")
	}
}

// 空值来自旧配置文件：Normalize 必须补成 auto，而不是留空让运行期猜。
func TestCompactResultTextNormalizeFillsEmpty(t *testing.T) {
	cfg := Default()
	cfg.Limits.CompactResultText = ""
	if got := Normalize(cfg).Limits.CompactResultText; got != ResultTextOff {
		t.Fatalf("空值应归一化为 %q（完整结果），得到 %q", ResultTextOff, got)
	}
}
