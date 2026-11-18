package model

import "testing"

func TestMatchProviderPresetsByHost(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{"带路径的官方地址", "https://api.mistral.ai/v1", true},
		{"裸域名", "https://mistral.ai", true},
		{"缺 scheme", "api.mistral.ai/v1", true},
		{"大写主机名", "https://API.Mistral.AI/v1", true},
		{"带端口", "https://api.mistral.ai:443/v1", true},
		{"仿冒域名", "https://api.mistral.ai.evil.com/v1", false},
		{"本机地址", "http://127.0.0.1:7897", false},
		{"别的厂商", "https://api.openai.com/v1", false},
		{"空串", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := len(MatchProviderPresets(tc.baseURL)) > 0
			if got != tc.want {
				t.Errorf("MatchProviderPresets(%q) 命中=%v，期望 %v", tc.baseURL, got, tc.want)
			}
		})
	}
}

func TestMistralPresetAppliesOnlyListedFields(t *testing.T) {
	preset, ok := LookupProviderPreset("mistral")
	if !ok {
		t.Fatal("内置预设里应有 mistral")
	}

	prov := Provider{
		ID:               11,
		Name:             "fagun",
		BaseURL:          "https://api.mistral.ai/v1",
		APIFormat:        FormatOpenAI,
		TimeoutSeconds:   0,
		UsageInjectMode:  ModeInherit,
		ContinuationMode: ContinuationOff,
	}
	prov.Keys = []APIKey{{ID: "k1", Key: "sk-secret"}}
	prov.Prompt.Text = "自定义提示词"

	applied := preset.Apply(&prov)
	if len(applied) != 4 {
		t.Fatalf("应改动四项（严格兼容/缓存键/用量注入/超时），实际 %v", applied)
	}
	if !prov.StripUnsupportedParams {
		t.Error("严格兼容应被打开")
	}
	if prov.PromptCacheKeyMode != PromptCacheKeyAuto {
		t.Errorf("prompt_cache_key 应为 auto，实际 %q", prov.PromptCacheKeyMode)
	}
	if prov.UsageInjectMode != "off" {
		t.Errorf("usage 注入应关闭，实际 %q", prov.UsageInjectMode)
	}
	if prov.TimeoutSeconds != 900 {
		t.Errorf("超时应为 900，实际 %d", prov.TimeoutSeconds)
	}
	// 预设不该碰这些。
	if len(prov.Keys) != 1 || prov.Keys[0].Key != "sk-secret" {
		t.Error("密钥不该被预设改动")
	}
	if prov.Prompt.Text != "自定义提示词" {
		t.Error("提示词不该被预设改动")
	}
	if prov.ContinuationMode != ContinuationOff {
		t.Error("续传本来就是 off，不该产生改动")
	}

	// 幂等：再应用一次没有新改动。
	if again := preset.Apply(&prov); len(again) != 0 {
		t.Errorf("重复应用不该再有改动，实际 %v", again)
	}
	if !preset.Applied(prov) {
		t.Error("应用之后 Applied 应为 true")
	}

	// 任何一项被改回去，都视为「未应用」。
	prov.UsageInjectMode = ModeInherit
	if preset.Applied(prov) {
		t.Error("有一项不符时 Applied 应为 false")
	}
}

func TestProviderPresetChangesMatchFields(t *testing.T) {
	preset, _ := LookupProviderPreset("mistral")
	// 界面靠 Changes 展示「改什么」，字段数量必须与要改的项对得上。
	if len(preset.Changes) != 5 {
		t.Errorf("应给出五条变更说明，实际 %d", len(preset.Changes))
	}
	for _, c := range preset.Changes {
		if c.Field == "" || c.Value == "" || c.Reason == "" {
			t.Errorf("变更说明不完整: %+v", c)
		}
	}
}
