package proxy

import (
	"context"
	"os"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

// 对真实账号跑一次用量查询。
//
// 这几个额度接口是上游未公开的，结构随时可能变；上面那些用例钉的是「照着实测样本
// 能解析对」，钉不住「上游今天还是这个结构」。所以留一个默认跳过的实查：
//
//	COMMAND_CODE_API_KEY=user_xxx go test ./internal/proxy -run Live -v
//
// 它只发 GET，不消耗额度。密钥只从环境变量读，不落任何文件。
func TestLiveCommandCodeUsage(t *testing.T) {
	key := os.Getenv("COMMAND_CODE_API_KEY")
	if key == "" {
		t.Skip("未设置 COMMAND_CODE_API_KEY，跳过实查")
	}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := NewServer(st, hub.New(), quietLogger())

	prov := model.Provider{
		Name:       "cc",
		Enabled:    true,
		BaseURL:    "https://api.commandcode.ai/provider/v1",
		APIFormat:  model.FormatOpenAI,
		Keys:       []model.APIKey{{ID: "k1", Key: key, Enabled: true}},
		UsageQuery: model.UsageQueryConfig{Template: "commandcode"},
	}
	settings, err := st.GetSettings()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	snap := srv.QueryUsage(ctx, prov, settings)

	if snap.Error != "" {
		t.Fatalf("整体失败: %s", snap.Error)
	}
	if !snap.OK {
		t.Fatalf("未读到额度，warnings=%v", snap.Warnings)
	}
	for _, w := range snap.Warnings {
		t.Logf("降级：%s", w)
	}
	t.Logf("账号=%s 计划=%s(%s) 状态=%s", snap.Account, snap.PlanName, snap.PlanID, snap.Status)
	for _, b := range snap.Balances {
		t.Logf("余额 %s = %.4f", b.Label, b.Amount)
	}
	for _, w := range snap.Windows {
		t.Logf("窗口 %s = %.4f / %.4f (%.1f%%) 重置=%v", w.Label, w.Used, w.Cap, w.Percent, w.ResetAt)
	}
	for _, s := range snap.Period {
		t.Logf("统计 %s = %v %s", s.Label, s.Value, s.Unit)
	}

	// 任何一段读不出来都值得知道 —— 上游大概率改了结构。
	if len(snap.Warnings) > 0 {
		t.Errorf("有 %d 段未读到，请对照 raw 检查上游是否改了结构", len(snap.Warnings))
	}
}
