package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"ai_proxy/internal/model"
)

// clientPool 缓存按配置指纹构造的 http.Client。
//
// 每次请求都新建 Transport 会丢掉连接复用，在高频调用下显著推高延迟；
// 但 Transport 的代理、超时、TLS 设置都来自可随时修改的配置，
// 所以这里按「配置指纹」做缓存，配置变了自然落到新的 key 上。
type clientPool struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

func newClientPool() *clientPool {
	return &clientPool{clients: map[string]*http.Client{}}
}

func (p *clientPool) get(key string, build func() (*http.Client, error)) (*http.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[key]; ok {
		return c, nil
	}
	c, err := build()
	if err != nil {
		return nil, err
	}
	// 简单上限，防止长期运行中配置反复微调导致缓存无限增长。
	if len(p.clients) > 64 {
		p.clients = map[string]*http.Client{}
	}
	p.clients[key] = c
	return c, nil
}

// reset 在全局设置变化后清空缓存。
func (p *clientPool) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clients = map[string]*http.Client{}
}

// resolvedNet 是本次请求最终采用的网络参数。
type resolvedNet struct {
	Proxy       model.ProxyConfig
	ProxyInUse  bool
	ProxyDesc   string        // 落进日志的一句话描述
	TotalTime   time.Duration // 0 表示不限制
	ConnectTime time.Duration
	InsecureTLS bool
}

// resolveNet 合并供应商级与全局级的网络配置。
func resolveNet(prov model.Provider, st model.Settings) resolvedNet {
	r := resolvedNet{InsecureTLS: prov.InsecureSkipTLS}

	total := prov.TimeoutSeconds
	if total == 0 {
		total = st.DefaultTimeoutSeconds
	}
	if total > 0 {
		r.TotalTime = time.Duration(total) * time.Second
	}

	connect := prov.ConnectTimeoutSeconds
	if connect == 0 {
		connect = st.DefaultConnectTimeoutSeconds
	}
	if connect <= 0 {
		connect = 15
	}
	r.ConnectTime = time.Duration(connect) * time.Second

	switch prov.ProxyMode {
	case "direct":
		// 明确不走代理
	case model.ModeCustom:
		if prov.Proxy.Enabled && prov.Proxy.URL != "" {
			r.Proxy = prov.Proxy
			r.ProxyInUse = true
			r.ProxyDesc = prov.Proxy.Type + "://" + prov.Proxy.URL
		}
	default: // inherit
		if st.Proxy.Enabled && st.Proxy.URL != "" {
			r.Proxy = st.Proxy
			r.ProxyInUse = true
			r.ProxyDesc = st.Proxy.Type + "://" + st.Proxy.URL
		}
	}
	return r
}

// proxySignature 生成代理配置的指纹，用作 client 缓存 key 的一部分。
func (r resolvedNet) proxySignature() string {
	if !r.ProxyInUse {
		return "none"
	}
	return strings.Join([]string{
		r.Proxy.Type, r.Proxy.Normalized(), r.Proxy.Username, r.Proxy.Password,
	}, "|")
}

// client 返回适用于本次请求的 http.Client。
func (p *clientPool) client(r resolvedNet) (*http.Client, error) {
	key := fmt.Sprintf("%s|%s|%v|%v", r.proxySignature(), r.ConnectTime, r.InsecureTLS,
		strings.Join(r.Proxy.NoProxy, ","))

	return p.get(key, func() (*http.Client, error) {
		transport := &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   r.ConnectTime,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   r.ConnectTime,
			ExpectContinueTimeout: 1 * time.Second,
			// SSE 场景下服务器会持续推送，不能压缩也不能缓冲。
			DisableCompression: true,
			ForceAttemptHTTP2:  true,
		}
		if r.InsecureTLS {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}

		if err := applyProxy(transport, r.Proxy); err != nil {
			return nil, err
		}

		// 不设 client.Timeout：总时长由每次请求的 context 控制，
		// 这样「不限制」语义才能真正生效（client.Timeout 无法表达无限）。
		return &http.Client{
			Transport: transport,
			// 转发层自己处理重定向语义更安全；上游 3xx 一般也是错误信号。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}, nil
	})
}

// applyProxy 把代理配置装到 Transport 上。
//
// http/https 代理走 Transport.Proxy（标准库会为 https 目标自动建立 CONNECT 隧道）；
// socks5 没有对应的原生支持，需要替换 DialContext。
func applyProxy(transport *http.Transport, cfg model.ProxyConfig) error {
	if !cfg.Enabled || cfg.URL == "" {
		return nil
	}

	if cfg.Type == model.ProxySOCKS5 {
		addr := stripScheme(cfg.Normalized())
		var auth *proxy.Auth
		if cfg.Username != "" || cfg.Password != "" {
			auth = &proxy.Auth{User: cfg.Username, Password: cfg.Password}
		}
		dialer, err := proxy.SOCKS5("tcp", addr, auth, &net.Dialer{
			Timeout:   transport.TLSHandshakeTimeout,
			KeepAlive: 30 * time.Second,
		})
		if err != nil {
			return fmt.Errorf("创建 SOCKS5 拨号器失败: %w", err)
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return fmt.Errorf("SOCKS5 拨号器不支持带 context 的拨号")
		}
		transport.DialContext = contextDialer.DialContext
		// SOCKS5 模式下不再设置 Proxy，否则会双重代理。
		return nil
	}

	u, err := url.Parse(cfg.Normalized())
	if err != nil {
		return fmt.Errorf("解析代理地址失败: %w", err)
	}
	if cfg.Username != "" {
		u.User = url.UserPassword(cfg.Username, cfg.Password)
	}
	noProxy := cfg.NoProxy
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		if matchesNoProxy(req.URL.Hostname(), noProxy) {
			return nil, nil
		}
		return u, nil
	}
	return nil
}

// stripScheme 去掉 URL 里的 scheme，SOCKS5 拨号器只接受 host:port。
func stripScheme(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		return raw[i+3:]
	}
	return raw
}

// matchesNoProxy 判断主机是否命中直连列表。匹配规则与常见 CLI 工具一致：
// 完整相等，或作为域名后缀（.example.com 及裸 example.com 都算命中）。
func matchesNoProxy(host string, list []string) bool {
	if len(list) == 0 {
		return false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	for _, raw := range list {
		item := strings.ToLower(strings.TrimSpace(raw))
		if item == "" {
			continue
		}
		item = strings.TrimPrefix(item, ".")
		if host == item || strings.HasSuffix(host, "."+item) {
			return true
		}
	}
	return false
}

// ---------- 密钥轮询与健康状态 ----------

// keyPool 维护每个供应商的轮询下标与密钥冷却状态。
//
// 健康信息只放在内存里：它描述的是「此刻哪个密钥还能用」，
// 重启后重新探测即可，没必要持久化，也就避免了每次请求都写库。
type keyPool struct {
	mu        sync.Mutex
	counter   map[int64]uint64
	cooldown  map[string]time.Time // key: providerID + "\x00" + keyID
	lastError map[string]string
}

func newKeyPool() *keyPool {
	return &keyPool{
		counter:   map[int64]uint64{},
		cooldown:  map[string]time.Time{},
		lastError: map[string]string{},
	}
}

func keyStateID(providerID int64, keyID string) string {
	return fmt.Sprintf("%d\x00%s", providerID, keyID)
}

// Pick 选出一个当前可用的密钥。
//
// 优先挑不在冷却期内的密钥做轮询；如果全都在冷却，退化为「挑一个冷却最早结束的」，
// 毕竟试一把总比直接失败好。
func (p *keyPool) Pick(prov model.Provider) (model.APIKey, bool) {
	enabled := make([]model.APIKey, 0, len(prov.Keys))
	for _, k := range prov.Keys {
		if k.Enabled && k.Key != "" {
			enabled = append(enabled, k)
		}
	}
	if len(enabled) == 0 {
		return model.APIKey{}, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	healthy := make([]model.APIKey, 0, len(enabled))
	for _, k := range enabled {
		if until, ok := p.cooldown[keyStateID(prov.ID, k.ID)]; ok && now.Before(until) {
			continue
		}
		healthy = append(healthy, k)
	}

	pool := healthy
	if len(pool) == 0 {
		// 全部处于冷却：挑冷却最早结束的那个。
		best := enabled[0]
		bestUntil := time.Time{}
		for _, k := range enabled {
			until := p.cooldown[keyStateID(prov.ID, k.ID)]
			if bestUntil.IsZero() || until.Before(bestUntil) {
				best, bestUntil = k, until
			}
		}
		pool = []model.APIKey{best}
	}

	idx := p.counter[prov.ID]
	p.counter[prov.ID] = idx + 1
	return pool[int(idx%uint64(len(pool)))], true
}

// Report 记录一次请求的结果，决定是否让该密钥进入冷却。
func (p *keyPool) Report(providerID int64, keyID string, status int, errMsg string) {
	if keyID == "" {
		return
	}
	id := keyStateID(providerID, keyID)

	p.mu.Lock()
	defer p.mu.Unlock()

	var cooldown time.Duration
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// 密钥本身有问题，冷却久一点，别把请求都浪费在它身上。
		cooldown = 5 * time.Minute
	case status == http.StatusTooManyRequests:
		cooldown = time.Minute
	case status == 0 && errMsg != "":
		// 连接层错误：可能是节点问题，短暂避让。
		cooldown = 30 * time.Second
	}

	if cooldown > 0 {
		p.cooldown[id] = time.Now().Add(cooldown)
		if errMsg != "" {
			p.lastError[id] = errMsg
		}
		return
	}
	// 成功则立刻恢复可用。
	delete(p.cooldown, id)
	delete(p.lastError, id)
}

// Health 返回密钥健康快照，供管理界面展示。
func (p *keyPool) Health(prov model.Provider) map[string]KeyHealth {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := map[string]KeyHealth{}
	now := time.Now()
	for _, k := range prov.Keys {
		id := keyStateID(prov.ID, k.ID)
		h := KeyHealth{}
		if until, ok := p.cooldown[id]; ok && now.Before(until) {
			h.Cooling = true
			h.RecoverAt = until
			h.CooldownMs = until.Sub(now).Milliseconds()
		}
		h.LastError = p.lastError[id]
		out[k.ID] = h
	}
	return out
}

// KeyHealth 是单个密钥的运行时健康状态。
type KeyHealth struct {
	Cooling    bool      `json:"cooling"`
	CooldownMs int64     `json:"cooldownMs"`
	RecoverAt  time.Time `json:"recoverAt"`
	LastError  string    `json:"lastError"`
}

// buildRequest 构造发往上游的请求。
func buildRequest(ctx context.Context, method, target string, body []byte, headers http.Header) (*http.Request, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("构造上游请求失败: %w", err)
	}
	// 直接接管请求头：调用方已经做过 hop-by-hop 剥离与认证头覆盖。
	req.Header = headers
	if len(body) > 0 {
		// 显式给出长度，避免退化成 chunked，部分网关对 chunked 支持不佳。
		req.ContentLength = int64(len(body))
	}
	return req, nil
}
