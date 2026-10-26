package proxy

// commandcode + 15 张图的真实代理链路测试，冷却时间配置为 0：
// 如果出现回退，下一轮必须立刻重新建链，而不是进入 unsupported。
//
//	CONTINUATION_LIVE=1 CONTINUATION_CC_LIVE=1 \
//	  go test ./internal/proxy -run TestLiveCommandCode15ImagesZeroCooldown -v
//
// 只读 data/ai_proxy.db 取 commandcode 的密钥/BaseURL 与代理设置；
// 用临时库启动进程内 ai_proxy，不碰正在运行的实例。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"

	_ "modernc.org/sqlite"
)

func TestLiveCommandCode15ImagesZeroCooldown(t *testing.T) {
	if os.Getenv("CONTINUATION_LIVE") != "1" || os.Getenv("CONTINUATION_CC_LIVE") != "1" {
		t.Skip("未设置 CONTINUATION_LIVE=1 与 CONTINUATION_CC_LIVE=1，跳过 commandcode 15 图实验")
	}

	key, baseURL, settings := liveCommandCodeTarget(t)
	modelName := os.Getenv("COMMAND_CODE_MODEL")
	if modelName == "" {
		modelName = "deepseek/deepseek-v4.1-flash"
	}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveSettings(settings); err != nil {
		t.Fatalf("写入临时设置失败: %v", err)
	}

	p := model.Provider{
		Name:                               "cc-cont-15",
		DisplayName:                        "cc-cont-15",
		Enabled:                            true,
		BaseURL:                            baseURL,
		APIFormat:                          model.FormatResponses,
		ContinuationMode:                   model.ContinuationOn,
		ContinuationModels:                 []string{modelName},
		ContinuationCooldownSeconds:        0,
		ContinuationInvalidCooldownSeconds: 0,
		Keys:                               []model.APIKey{{ID: "live", Key: key, Enabled: true}},
	}
	if _, err := st.CreateProvider(p); err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	srv := NewServer(st, hub.New(), quietLogger())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := &http.Client{Timeout: 300 * time.Second}
	path := "/p/cc-cont-15/v1/responses"

	question1 := "第 7 张图（按图片顺序从 1 开始数）的主色是什么？只回答颜色名称，不要解释。"
	question2 := "第 15 张图（按图片顺序从 1 开始数）的主色是什么？只回答颜色名称，不要解释。"

	const reps = 3
	for rep := 1; rep <= reps; rep++ {
		session := fmt.Sprintf("cc-15-zero-%d-%d", rep, time.Now().UnixNano())
		headers := continuationProxyHeaders(settings.AccessKey, session)
		baseImgs := generatedImagesForRep(t, rep)
		imgs := make([]continuationImage, 15)
		for i := range imgs {
			imgs[i] = baseImgs[i%len(baseImgs)]
		}

		body1, input1 := continuationTurn1Body(modelName, session, imgs)
		body1 = continuationProxyStoreFalse(t, body1)
		first := sendContinuationRequest(t, client, ts.URL+path, headers, body1)
		if first.status != http.StatusOK {
			t.Fatalf("rep %d 首轮返回 %d: %s", rep, first.status, continuationTrim(first.body, 500))
		}
		log1 := lastLog(t, st)
		output1 := first.output
		if len(output1) == 0 {
			output1 = []json.RawMessage{mustJSON(contAssistant(first.text))}
		}

		body2 := continuationFullTurn2Body(modelName, session, input1, output1, question1, false)
		second := sendContinuationRequest(t, client, ts.URL+path, headers, body2)
		if second.status != http.StatusOK {
			t.Fatalf("rep %d 第二轮返回 %d: %s", rep, second.status, continuationTrim(second.body, 500))
		}
		log2 := lastLog(t, st)
		output2 := second.output
		if len(output2) == 0 {
			output2 = []json.RawMessage{mustJSON(contAssistant(second.text))}
		}
		if log2.ContinuationMode == "fallback" {
			// 冷却 0：第三轮必须立刻重建链，不能 unsupported。
			// 这里只记录，第三轮之后统一断言。
		}

		turn2Input := append([]any{}, input1...)
		// 按真实客户端形态回放：ZCode 不带 reasoning 项。
		turn2Input = append(turn2Input, replayOutputItems(output1)...)
		turn2Input = append(turn2Input, contUser(question1))
		body3 := continuationFullTurn2Body(modelName, session, turn2Input, output2, question2, false)
		third := sendContinuationRequest(t, client, ts.URL+path, headers, body3)
		if third.status != http.StatusOK {
			t.Fatalf("rep %d 第三轮返回 %d: %s", rep, third.status, continuationTrim(third.body, 500))
		}
		log3 := lastLog(t, st)

		if log2.ContinuationMode == "fallback" && log3.ContinuationMode == "unsupported" {
			t.Errorf("rep %d：冷却 0 时回退后的下一轮不应是 unsupported（实际 %q/%q）",
				rep, log3.ContinuationMode, log3.ContinuationReason)
		}
		if !continuationMatchesColor(second.text, "青色") {
			t.Errorf("rep %d 第二轮应回答青色，实际 %q", rep, continuationTrim(second.text, 100))
		}
		if !continuationMatchesColor(third.text, "蓝色") {
			t.Errorf("rep %d 第三轮应回答蓝色，实际 %q", rep, continuationTrim(third.text, 100))
		}

		t.Logf("rep %d：turn1 mode=%s/%s ttft=%dms；turn2 mode=%s/%s ttft=%dms saved=%d recall=%v；"+
			"turn3 mode=%s/%s ttft=%dms saved=%d recall=%v",
			rep,
			log1.ContinuationMode, log1.ContinuationReason, log1.TTFTMs,
			log2.ContinuationMode, log2.ContinuationReason, log2.TTFTMs, log2.SavedBytes,
			continuationMatchesColor(second.text, "青色"),
			log3.ContinuationMode, log3.ContinuationReason, log3.TTFTMs, log3.SavedBytes,
			continuationMatchesColor(third.text, "蓝色"))
	}
}

// liveCommandCodeTarget 从 DB 读 commandcode 供应商；密钥只留在内存里。
func liveCommandCodeTarget(t *testing.T) (key, baseURL string, settings model.Settings) {
	t.Helper()

	baseURL = os.Getenv("COMMAND_CODE_BASE_URL")
	path := os.Getenv("AI_PROXY_DB")
	if path == "" {
		path = filepath.Join("..", "..", "data", "ai_proxy.db")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("解析库路径失败: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(abs, `\`, "/")+"?mode=ro")
	if err != nil {
		t.Fatalf("打开只读数据库失败: %v", err)
	}
	defer db.Close()

	var raw string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'global'`).Scan(&raw); err == nil {
		if json.Unmarshal([]byte(raw), &settings) == nil {
			settings.ApplyDefaults()
		}
	}

	if key = os.Getenv("COMMAND_CODE_API_KEY"); key != "" {
		if baseURL == "" {
			baseURL = "https://api.commandcode.ai/provider/v1"
		}
		return key, baseURL, settings
	}

	var dbBase, keysJSON string
	err = db.QueryRow(`SELECT base_url, keys_json FROM providers
		WHERE display_name LIKE '%commandcode%' AND enabled = 1
		ORDER BY active DESC, sort_order, id LIMIT 1`).Scan(&dbBase, &keysJSON)
	if err == nil {
		if baseURL == "" {
			baseURL = dbBase
		}
		var keys []model.APIKey
		if json.Unmarshal([]byte(keysJSON), &keys) == nil {
			for _, k := range keys {
				if k.Enabled && strings.TrimSpace(k.Key) != "" {
					key = k.Key
					break
				}
			}
		}
	}
	if key == "" {
		t.Skip("没有 COMMAND_CODE_API_KEY，数据库里也没有可用的 commandcode 供应商密钥")
	}
	if baseURL == "" {
		baseURL = "https://api.commandcode.ai/provider/v1"
	}
	return key, baseURL, settings
}
