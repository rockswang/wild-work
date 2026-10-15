package app

import (
	"encoding/json"
	"os"
	"testing"
)

// 面板侧「思考强度注入映射」的保存/校验/回显回归（对齐模型降级链 POST
// /api/config/model_fallback 的端点契约）。注入转发行为由 internal/server
// 的 handler_effort_test.go 锁死，这里只验配置面与 API 契约。

func TestSaveReasoningEffortsRoundTrip(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)

	// 保存：落字段 + 落盘 + state 回显
	if w := doReq(mux, "POST", "/api/config/reasoning_efforts", `{"efforts":{"workbuddy/glm-5.2":"high","*":"medium"}}`, nil); w.Code != 200 {
		t.Fatalf("保存应 200：%d %s", w.Code, w.Body.String())
	}
	if a.cfg.ReasoningEfforts["workbuddy/glm-5.2"] != "high" || a.cfg.ReasoningEfforts["*"] != "medium" {
		t.Errorf("配置未落字段：%v", a.cfg.ReasoningEfforts)
	}
	raw, err := os.ReadFile(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		ReasoningEfforts map[string]string `json:"reasoning_efforts"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil { // config.Save 是缩进格式，故按字段断言而非子串
		t.Fatal(err)
	}
	if onDisk.ReasoningEfforts["*"] != "medium" || onDisk.ReasoningEfforts["workbuddy/glm-5.2"] != "high" {
		t.Errorf("配置文件的 reasoning_efforts 未落盘：%s", raw)
	}
	var st State
	if err := json.Unmarshal(doReq(mux, "GET", "/api/state", "", nil).Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.ReasoningEfforts["*"] != "medium" {
		t.Errorf("state 回显错误：%v", st.ReasoningEfforts)
	}

	// 校验：空键 / 空值拒绝，且被拒保存不得改动现有配置
	if w := doReq(mux, "POST", "/api/config/reasoning_efforts", `{"efforts":{"":"low"}}`, nil); w.Code != 400 {
		t.Errorf("空模型键应 400，实得 %d %s", w.Code, w.Body.String())
	}
	if w := doReq(mux, "POST", "/api/config/reasoning_efforts", `{"efforts":{"workbuddy/glm-5.2":"  "}}`, nil); w.Code != 400 {
		t.Errorf("空档位应 400，实得 %d %s", w.Code, w.Body.String())
	}
	if a.cfg.ReasoningEfforts["*"] != "medium" || len(a.cfg.ReasoningEfforts) != 2 {
		t.Errorf("被拒的保存不得改动现有配置：%v", a.cfg.ReasoningEfforts)
	}

	// trim：键值首尾空白在保存时剥掉
	if w := doReq(mux, "POST", "/api/config/reasoning_efforts", `{"efforts":{"  workbuddy/glm-5.2  ":"  high  "}}`, nil); w.Code != 200 {
		t.Fatalf("带空白键值应 trim 后保存成功：%d %s", w.Code, w.Body.String())
	}
	if a.cfg.ReasoningEfforts["workbuddy/glm-5.2"] != "high" || len(a.cfg.ReasoningEfforts) != 1 {
		t.Errorf("trim 后应只剩规范化键值：%v", a.cfg.ReasoningEfforts)
	}

	// 空 map = 清空禁用
	if w := doReq(mux, "POST", "/api/config/reasoning_efforts", `{"efforts":{}}`, nil); w.Code != 200 {
		t.Fatalf("空 map 清空应 200：%d", w.Code)
	}
	if len(a.cfg.ReasoningEfforts) != 0 {
		t.Errorf("空 map 应清空映射：%v", a.cfg.ReasoningEfforts)
	}
	raw, err = os.ReadFile(a.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk2 struct {
		ReasoningEfforts map[string]string `json:"reasoning_efforts"`
	}
	_ = json.Unmarshal(raw, &onDisk2)
	if len(onDisk2.ReasoningEfforts) != 0 {
		t.Errorf("清空后落盘应为空：%s", raw)
	}
}
