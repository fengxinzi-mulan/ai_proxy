package proxy

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai_proxy/internal/model"
)

func TestMatchesNoProxy(t *testing.T) {
	list := []string{"example.com", ".internal.corp", " localhost "}

	cases := map[string]bool{
		"example.com":         true,
		"api.example.com":     true, // 子域后缀命中
		"internal.corp":       true, // 前导点写法也应命中裸域名
		"a.internal.corp":     true,
		"localhost":           true,
		"notexample.com":      false,
		"example.com.evil.io": false,
		"other.io":            false,
	}
	for host, want := range cases {
		if got := matchesNoProxy(host, list); got != want {
			t.Errorf("matchesNoProxy(%q) 期望 %v，实际 %v", host, want, got)
		}
	}
	if matchesNoProxy("example.com", nil) {
		t.Error("空列表不应命中")
	}
}

func TestResolveNet(t *testing.T) {
	global := model.Settings{
		Proxy:                        model.ProxyConfig{Enabled: true, Type: model.ProxyHTTP, URL: "127.0.0.1:7890"},
		DefaultTimeoutSeconds:        600,
		DefaultConnectTimeoutSeconds: 15,
	}

	// inherit：用全局代理
	prov := model.Provider{ProxyMode: model.ModeInherit}
	got := resolveNet(prov, global)
	if !got.ProxyInUse || got.Proxy.URL != "127.0.0.1:7890" {
		t.Errorf("inherit 应使用全局代理: %+v", got)
	}
	if got.TotalTime != 600*time.Second {
		t.Errorf("应回落到全局默认超时，实际 %v", got.TotalTime)
	}

	// direct：绕过全局代理
	prov = model.Provider{ProxyMode: "direct"}
	got = resolveNet(prov, global)
	if got.ProxyInUse {
		t.Error("direct 模式不该使用代理")
	}

	// custom：用自己的代理
	prov = model.Provider{
		ProxyMode:      model.ModeCustom,
		Proxy:          model.ProxyConfig{Enabled: true, Type: model.ProxySOCKS5, URL: "10.0.0.1:1080"},
		TimeoutSeconds: 30,
	}
	got = resolveNet(prov, global)
	if !got.ProxyInUse || got.Proxy.Type != model.ProxySOCKS5 {
		t.Errorf("custom 应使用供应商自己的代理: %+v", got)
	}
	if got.TotalTime != 30*time.Second {
		t.Errorf("供应商级超时应优先，实际 %v", got.TotalTime)
	}

	// 自定义代理但未启用：视为不使用
	prov = model.Provider{ProxyMode: model.ModeCustom, Proxy: model.ProxyConfig{Enabled: false, URL: "x:1"}}
	if resolveNet(prov, global).ProxyInUse {
		t.Error("代理未启用时不应使用")
	}

	// 全局关闭代理
	off := model.Settings{Proxy: model.ProxyConfig{Enabled: false, URL: "127.0.0.1:7890"}}
	if resolveNet(model.Provider{}, off).ProxyInUse {
		t.Error("全局代理关闭时不应用于 inherit")
	}
}

func TestProxyConfigNormalized(t *testing.T) {
	cases := []struct {
		cfg  model.ProxyConfig
		want string
	}{
		{model.ProxyConfig{Type: model.ProxyHTTP, URL: "127.0.0.1:7890"}, "http://127.0.0.1:7890"},
		{model.ProxyConfig{Type: model.ProxyHTTP, URL: "http://127.0.0.1:7890"}, "http://127.0.0.1:7890"},
		{model.ProxyConfig{Type: model.ProxySOCKS5, URL: "127.0.0.1:1080"}, "socks5://127.0.0.1:1080"},
		{model.ProxyConfig{Type: model.ProxySOCKS5, URL: "socks5://127.0.0.1:1080"}, "socks5://127.0.0.1:1080"},
		{model.ProxyConfig{Type: model.ProxyHTTP, URL: ""}, ""},
	}
	for _, c := range cases {
		if got := c.cfg.Normalized(); got != c.want {
			t.Errorf("Normalized() 期望 %q，实际 %q", c.want, got)
		}
	}
}

// TestHTTPProxyIsUsed 验证 http 代理确实参与了连接：目标地址不可达，
// 但代理可达 —— 请求能否成功取决于代理是否被使用。
func TestHTTPProxyIsUsed(t *testing.T) {
	var proxyHits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		// 代理看到的是绝对 URI 形式的请求。
		if !strings.HasPrefix(r.URL.String(), "http://") {
			t.Errorf("经代理的请求应带绝对 URI，实际 %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"via":"proxy","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer proxy.Close()

	srv, st := newTestServer(t)
	addProvider(t, st, "http://upstream.invalid", model.FormatOpenAI)
	saveSettings(t, st, func(s *model.Settings) {
		s.Proxy = model.ProxyConfig{Enabled: true, Type: model.ProxyHTTP, URL: proxy.URL}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应通过代理拿到 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&proxyHits) != 1 {
		t.Errorf("代理应被访问 1 次，实际 %d", proxyHits)
	}
	entry := lastLog(t, st)
	if entry.ProxyUsed == "" {
		t.Error("日志应记录实际使用的代理")
	}
}

// TestSOCKS5ProxyIsUsed 用一个极简的 SOCKS5 服务端验证 socks5 链路真的通了。
// 这是「全局代理支持 http 与 socks5」这条需求的核心验证。
func TestSOCKS5ProxyIsUsed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"via":"socks5","usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer upstream.Close()

	socksAddr, connects := startTestSOCKS5(t)

	srv, st := newTestServer(t)
	addProvider(t, st, upstream.URL, model.FormatOpenAI)
	saveSettings(t, st, func(s *model.Settings) {
		s.Proxy = model.ProxyConfig{Enabled: true, Type: model.ProxySOCKS5, URL: socksAddr}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应经 SOCKS5 拿到 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "socks5") {
		t.Errorf("响应未来自目标上游: %s", rec.Body.String())
	}
	if atomic.LoadInt32(connects) == 0 {
		t.Error("SOCKS5 代理未被使用")
	}
	entry := lastLog(t, st)
	if entry.PromptTokens != 3 || entry.CompletionTokens != 2 {
		t.Errorf("经代理的统计应正常: %+v", entry)
	}
	if !strings.Contains(entry.ProxyUsed, "socks5") {
		t.Errorf("日志应记录 socks5 代理，实际 %q", entry.ProxyUsed)
	}
}

// startTestSOCKS5 启动一个只支持无认证 CONNECT 的最小 SOCKS5 服务端。
// 返回监听地址与已处理的连接计数。
func startTestSOCKS5(t *testing.T) (string, *int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听端口失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var connects int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// 在 CONNECT 成功的瞬间计数，而不是等隧道关闭 ——
				// 后者会让断言与转发完成之间存在竞态。
				handleSOCKS5(c, func() { atomic.AddInt32(&connects, 1) })
			}(conn)
		}
	}()
	return ln.Addr().String(), &connects
}

// handleSOCKS5 处理一次 SOCKS5 会话，onConnect 在隧道建立后回调。
func handleSOCKS5(c net.Conn, onConnect func()) bool {
	c.SetDeadline(time.Now().Add(10 * time.Second))

	// 握手：VER, NMETHODS, METHODS...
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil || head[0] != 0x05 {
		return false
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return false
	}
	// 选择「无认证」。
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return false
	}

	// 请求：VER, CMD, RSV, ATYP, DST.ADDR, DST.PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 0x01 {
		return false // 只支持 CONNECT
	}
	var host string
	switch req[3] {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return false
		}
		host = net.IP(b).String()
	case 0x03:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(c, lb); err != nil {
			return false
		}
		nb := make([]byte, lb[0])
		if _, err := io.ReadFull(c, nb); err != nil {
			return false
		}
		host = string(nb)
	default:
		return false
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(c, portBytes); err != nil {
		return false
	}
	dst := net.JoinHostPort(host, itoa(binary.BigEndian.Uint16(portBytes)))

	target, err := net.DialTimeout("tcp", dst, 5*time.Second)
	if err != nil {
		c.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return false
	}
	defer target.Close()

	// 成功应答：用 IPv4 的 0.0.0.0:0 表示「由代理自行填充」。
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return false
	}
	if onConnect != nil {
		onConnect()
	}
	c.SetDeadline(time.Time{})

	done := make(chan struct{}, 2)
	go func() { io.Copy(target, c); done <- struct{}{} }()
	go func() { io.Copy(c, target); done <- struct{}{} }()
	<-done
	return true
}

func itoa(v uint16) string {
	if v == 0 {
		return "0"
	}
	var buf [5]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// ---------- 多密钥轮询与冷却 ----------

func TestKeyPoolRoundRobin(t *testing.T) {
	p := model.Provider{ID: 1, Keys: []model.APIKey{
		{ID: "a", Key: "ka", Enabled: true},
		{ID: "b", Key: "kb", Enabled: true},
		{ID: "c", Key: "kc", Enabled: false}, // 停用
	}}
	pool := newKeyPool()

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		k, ok := pool.Pick(p)
		if !ok {
			t.Fatal("应能选出密钥")
		}
		seen[k.ID]++
	}
	if seen["a"] != 3 || seen["b"] != 3 {
		t.Errorf("应在启用的密钥间均匀轮询，实际 %v", seen)
	}
	if seen["c"] != 0 {
		t.Error("停用的密钥不该被选中")
	}
}

func TestKeyPoolNoKeys(t *testing.T) {
	pool := newKeyPool()
	if _, ok := pool.Pick(model.Provider{ID: 1}); ok {
		t.Error("没有密钥时应返回 false")
	}
}

func TestKeyPoolCooldown(t *testing.T) {
	p := model.Provider{ID: 7, Keys: []model.APIKey{
		{ID: "bad", Key: "k1", Enabled: true},
		{ID: "good", Key: "k2", Enabled: true},
	}}
	pool := newKeyPool()
	// 先轮询到 bad，再报告 401，它应进入冷却。
	pool.Report(p.ID, "bad", http.StatusUnauthorized, "invalid api key")

	for i := 0; i < 4; i++ {
		k, _ := pool.Pick(p)
		if k.ID == "bad" {
			t.Fatal("处于冷却期的密钥不该被选中")
		}
	}

	h := pool.Health(p)
	if !h["bad"].Cooling {
		t.Error("失败密钥应显示为冷却中")
	}
	if h["bad"].LastError == "" {
		t.Error("应记录失败原因")
	}
	if h["good"].Cooling {
		t.Error("正常密钥不应被标记冷却")
	}

	// 成功一次后应立刻恢复可用。
	pool.Report(p.ID, "bad", http.StatusOK, "")
	if pool.Health(p)["bad"].Cooling {
		t.Error("成功上报后应解除冷却")
	}
}

// TestKeyPoolAllCoolingStillPicks 保证「全部密钥都在冷却」时仍会尝试，
// 而不是直接失败 —— 宁可试一把也不要让用户干等。
func TestKeyPoolAllCoolingStillPicks(t *testing.T) {
	p := model.Provider{ID: 9, Keys: []model.APIKey{
		{ID: "a", Key: "k1", Enabled: true},
		{ID: "b", Key: "k2", Enabled: true},
	}}
	pool := newKeyPool()
	pool.Report(p.ID, "a", http.StatusTooManyRequests, "rate limited")
	pool.Report(p.ID, "b", http.StatusTooManyRequests, "rate limited")

	k, ok := pool.Pick(p)
	if !ok {
		t.Fatal("全部冷却时仍应挑一个来试")
	}
	if k.ID != "a" && k.ID != "b" {
		t.Errorf("应选出已配置的密钥，实际 %s", k.ID)
	}
}

func TestApplyProxyNoProxyBypass(t *testing.T) {
	transport := &http.Transport{}
	err := applyProxy(transport, model.ProxyConfig{
		Enabled: true,
		Type:    model.ProxyHTTP,
		URL:     "127.0.0.1:7890",
		NoProxy: []string{"internal.corp"},
	})
	if err != nil {
		t.Fatalf("装配代理失败: %v", err)
	}

	bypass, err := transport.Proxy(&http.Request{URL: mustURL(t, "http://api.internal.corp/v1")})
	if err != nil {
		t.Fatalf("代理函数出错: %v", err)
	}
	if bypass != nil {
		t.Error("命中 no_proxy 的域名应直连")
	}

	proxied, err := transport.Proxy(&http.Request{URL: mustURL(t, "https://api.openai.com/v1")})
	if err != nil {
		t.Fatalf("代理函数出错: %v", err)
	}
	if proxied == nil || proxied.Host != "127.0.0.1:7890" {
		t.Errorf("普通域名应走代理，实际 %v", proxied)
	}
}

func TestApplyProxyWithCredentials(t *testing.T) {
	transport := &http.Transport{}
	err := applyProxy(transport, model.ProxyConfig{
		Enabled: true, Type: model.ProxyHTTP, URL: "proxy.example.com:8080",
		Username: "user", Password: "pass",
	})
	if err != nil {
		t.Fatalf("装配代理失败: %v", err)
	}
	u, err := transport.Proxy(&http.Request{URL: mustURL(t, "https://api.openai.com")})
	if err != nil {
		t.Fatalf("代理函数出错: %v", err)
	}
	pw, _ := u.User.Password()
	if u.User.Username() != "user" || pw != "pass" {
		t.Errorf("代理凭据未装配: %v", u.User)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析 URL 失败: %v", err)
	}
	return u
}

func TestStripScheme(t *testing.T) {
	if got := stripScheme("socks5://127.0.0.1:1080"); got != "127.0.0.1:1080" {
		t.Errorf("应剥掉 scheme，实际 %q", got)
	}
	if got := stripScheme("127.0.0.1:1080"); got != "127.0.0.1:1080" {
		t.Errorf("无 scheme 时应原样返回，实际 %q", got)
	}
}
