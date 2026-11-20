package proxy

import (
	"strings"
	"testing"

	"ai_proxy/internal/model"
)

// Anthropic 官方接口缺 anthropic-version 会直接 400，探活会把它误报成「连不通」。
// 但用户显式配过这个头时必须尊重用户的设置。
func TestProbeHeadersAddsAnthropicVersion(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{"缺省时自动补", nil, "2023-06-01"},
		{"用户自定义值优先", map[string]string{"anthropic-version": "2024-01-01"}, "2024-01-01"},
		{"大小写不敏感", map[string]string{"Anthropic-Version": "2023-01-01"}, "2023-01-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := baseProvider()
			p.APIFormat = model.FormatAnthropic
			p.ExtraHeaders = tc.extra
			p.Keys = []model.APIKey{{ID: "k1", Key: "sk-ant", Enabled: true}}

			h := probeHeaders(p)
			if got := h.Get("anthropic-version"); got != tc.want {
				t.Errorf("anthropic-version 应为 %q，实际 %q", tc.want, got)
			}
			if got := h.Get("x-api-key"); got != "sk-ant" {
				t.Errorf("认证头不该被影响，实际 %q", got)
			}
		})
	}

	// 其它格式不加：有的网关会因为这个多余的头拒绝请求。
	p := baseProvider()
	if h := probeHeaders(p); h.Get("anthropic-version") != "" {
		t.Errorf("非 Anthropic 格式不该补 anthropic-version，实际 %q", h.Get("anthropic-version"))
	}
}

// parseModelList 只能把真正的模型 ID 捞出来：容器对象上的 name 是列表标题，
// 模型对象上的 name 常常是给人看的展示名。
func TestParseModelList(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "OpenAI 列表",
			body: `{"object":"list","data":[{"id":"gpt-4o","object":"model"},{"id":"gpt-4o-mini","object":"model"}]}`,
			want: []string{"gpt-4o", "gpt-4o-mini"},
		},
		{
			name: "id 存在时 name 是展示名",
			body: `{"data":[{"id":"m1","name":"Model One"},{"id":"m2","name":"Model Two"}]}`,
			want: []string{"m1", "m2"},
		},
		{
			name: "容器对象的名字不当模型",
			body: `{"object":"list","name":"Available Models","data":[{"id":"only"}]}`,
			want: []string{"only"},
		},
		{
			name: "Gemini 的 models 前缀要剥掉",
			body: `{"models":[{"name":"models/gemini-2.0-flash","displayName":"Gemini 2.0 Flash"},` +
				`{"name":"models/gemini-2.0-pro","displayName":"Gemini 2.0 Pro"}]}`,
			want: []string{"gemini-2.0-flash", "gemini-2.0-pro"},
		},
		{
			name: "model 字段兜底",
			body: `[{"model":"a"},{"model":"b"}]`,
			want: []string{"a", "b"},
		},
		{
			name: "非 JSON",
			body: `not json`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseModelList([]byte(tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("模型列表应为 %v，实际 %v", tc.want, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("模型列表应为 %v，实际 %v", tc.want, got)
				}
			}
		})
	}

	// 展示名一个都不该混进来。
	got := strings.Join(parseModelList([]byte(`{"data":[{"id":"m1","name":"Model One"}]}`)), ",")
	if strings.Contains(got, "Model One") {
		t.Errorf("展示名不该被当成模型 ID: %q", got)
	}
}
