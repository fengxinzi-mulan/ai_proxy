package proxy

// 用量快照刷新的写库纪律。
//
// 关键点：定时刷新与手动刷新共用 RefreshUsage，取消/超时属于被动中断，
// 它的结果不能覆盖库里已有的有效快照 —— 否则关页面、重启、切供应商都会把
// 「上一份好数据」冲成一条「查询失败」。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ai_proxy/internal/model"
)

func TestRefreshUsageSkipsSaveWhenContextCanceled(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, map[string]any{})
	}))
	t.Cleanup(upstream.Close)

	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, upstream.URL+"/provider/v1", true)

	// 先落一份「有效」的快照。
	good := model.UsageSnapshot{
		ProviderID: prov.ID,
		Template:   "commandcode",
		OK:         true,
		PlanName:   "旧的成功快照",
		FetchedAt:  time.Now().Add(-time.Hour),
	}
	if err := st.SaveProviderUsage(good); err != nil {
		t.Fatalf("保存初始快照失败: %v", err)
	}

	settings, err := st.GetSettings()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv.RefreshUsage(ctx, prov, settings)

	stored, err := st.GetProviderUsage(prov.ID)
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if stored == nil {
		t.Fatal("取消后不该把已有快照删掉")
	}
	if !stored.OK || stored.PlanName != "旧的成功快照" {
		t.Errorf("取消后的结果不该覆盖已有快照，实际 %+v", stored)
	}
}

func TestRefreshUsageSavesWhenContextAlive(t *testing.T) {
	// 正常路径要照常落库（否则上面那条「不覆盖」就没意义了）。
	mock := &ccMock{}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, upstream.URL+"/provider/v1", true)

	settings, err := st.GetSettings()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	snap := srv.RefreshUsage(ctx, prov, settings)
	if !snap.OK {
		t.Fatalf("查询应成功: %s %v", snap.Error, snap.Warnings)
	}
	stored, err := st.GetProviderUsage(prov.ID)
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if stored == nil || !stored.OK {
		t.Fatalf("正常刷新应落库，实际 %+v", stored)
	}
}
