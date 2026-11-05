package service

import "testing"

func TestAutoBackgroundWaitSecondsParse(t *testing.T) {
	cases := []struct {
		args map[string]any
		want int
		ok   bool
	}{
		{nil, 0, false},
		{map[string]any{}, 0, false},
		{map[string]any{"autoBackgroundSeconds": float64(30)}, 30, true},
		{map[string]any{"autoBackgroundSeconds": 5}, 5, true},
		{map[string]any{"autoBackgroundSeconds": float64(0)}, 0, false},
		{map[string]any{"autoBackgroundSeconds": float64(-3)}, 0, false},
		{map[string]any{"autoBackgroundSeconds": "30"}, 0, false},
	}
	for _, tc := range cases {
		got, ok := autoBackgroundWaitSeconds(tc.args)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("args=%v 期望 (%d,%v)，得到 (%d,%v)", tc.args, tc.want, tc.ok, got, ok)
		}
	}
}
