// oauth_missing_test.go「拿不到 OAuth 客户端凭据」与「网络不可达」两条路径的可操作性回归测试。
//
// ⚠️ 本文件**不用** main_test.go 的 TestMain 假凭据：那两个用例的前提恰恰是
// 「env 未设置 + 本机没有官方 CLI」。故这里显式清空 env，并把 GEMINI_CLI_HOME 指到
// 一个不存在的目录（否则开发机真装了 CLI 时用例会依赖机器状态）。
// 全部本机 httptest，不发任何真实网络请求（AGENTS.md §0.1）。
package login_gemini

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/gemini"
)

// clearOAuthClientEnv 让 gemini.ClientID()/ClientSecret() 在这一进程返回空串，用例结束后还原。
//
// ⚠️ 关键坑（实测踩出来的，两次）：gemini 的解析结果被 sync.Once 缓存、**进程级只算一次**。
// 若本文件的用例把"空凭据 + 空扫描结果"钉进缓存却不还原，后续所有依赖 TestMain 假凭据的
// 用例都会集体失败（表现：单跑本文件全绿、整包跑全红）。
//
// 还原必须**先恢复 env、再清缓存**：顺序反了的话，清理函数里的重置会把缓存钉成
// "仍未解析"，而 t.Setenv 的自动还原排在它之后——等真正轮到下一个用例时，缓存的
// 仍是"未解析"，看起来无害；真正致命的是还原顺序导致下一个用例首次解析时读到空 env。
// 所以两者的清理顺序在这里是**显式约定**的，别随手调换。
func clearOAuthClientEnv(t *testing.T) {
	t.Helper()
	oldID, idSet := os.LookupEnv(gemini.EnvClientID)
	oldSecret, secretSet := os.LookupEnv(gemini.EnvClientSecret)
	oldHome, homeSet := os.LookupEnv("GEMINI_CLI_HOME")

	_ = os.Unsetenv(gemini.EnvClientID)
	_ = os.Unsetenv(gemini.EnvClientSecret)
	// 指向不存在的目录：提取器必然返回空串，用例结果不依赖本机是否装了 CLI。
	_ = os.Setenv("GEMINI_CLI_HOME", filepath.Join(t.TempDir(), "no-cli-here"))
	// 清缓存后再读，才能拿到"空凭据"这一真实状态。
	gemini.ResetOAuthClientCacheForTest()

	t.Cleanup(func() {
		// 顺序硬约束：先恢复 env，再清缓存。
		// 反过来的话，清缓存会让下一次解析发生在 env 尚未恢复时 —— 实测就是这顺序
		// 让"空凭据"漏给了后续用例（-shuffle=on 与 -count=3 都能复现）。
		restore := func(k, v string, ok bool) {
			if ok {
				_ = os.Setenv(k, v)
			} else {
				_ = os.Unsetenv(k)
			}
		}
		restore(gemini.EnvClientID, oldID, idSet)
		restore(gemini.EnvClientSecret, oldSecret, secretSet)
		restore("GEMINI_CLI_HOME", oldHome, homeSet)
		gemini.ResetOAuthClientCacheForTest()
	})
}

// TestStartMissingOAuthClientIsActionable 守门（2026-10-17 现场缺陷）：
// 拿不到凭据时用户点「＋Gemini」只会看到一个 3 秒的 toast，文案还必须能**指导操作**。
//
// 修复前的文案只说了「设置 CMDC_PAK_GOOGLE_CLIENT_ID/SECRET」——用户照做仍然失败：
// 那两个值既没有给出获取途径，也没告诉用户「自己建 OAuth 客户端必须把回调地址注册成
// http://localhost:<随机端口>/oauth-callback」（不满足就是 redirect_uri_mismatch）。
// 更糟的是，它连「本机到底查了哪些位置」都不说，用户无从判断是不是装错了目录。
func TestStartMissingOAuthClientIsActionable(t *testing.T) {
	clearOAuthClientEnv(t)
	if gemini.ClientID() != "" || gemini.ClientSecret() != "" {
		t.Fatalf("前提不成立：本用例要求 ClientID/ClientSecret 为空，实际 %q/%q",
			gemini.ClientID(), gemini.ClientSecret())
	}

	statePath := filepath.Join(t.TempDir(), "login-state.json")
	authURL, err := Start(NewClient(), statePath)
	if err == nil {
		t.Fatalf("空凭据时必须报错，却返回了授权 URL：%s", authURL)
	}
	if !errors.Is(err, ErrOAuthClientMissing) {
		t.Fatalf("必须是 ErrOAuthClientMissing（app/前端按它给提示）：%v", err)
	}
	if authURL != "" {
		t.Errorf("报错时不得返回授权 URL，得 %q", authURL)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Errorf("报错时不得落 state 文件：%v", statErr)
	}

	msg := err.Error()
	// ① 必须点名「查了哪些位置」，用户才能自查（修复前一个字都没有）。
	// 断言用"扫描过"这个句式 + 一个必然存在的路径片段（node_modules），而不是 GEMINI_CLI_HOME：
	// 后者只在用户**真的设了**该变量时才会出现在路径列表里。
	if !strings.Contains(msg, "已查过的本机位置") || !strings.Contains(msg, "node_modules") {
		t.Errorf("文案应列出扫描过的 CLI 路径，实际：%s", msg)
	}
	// ② 必须给出**可执行**的获取途径，而不只是变量名。
	for _, want := range []string{
		"npm install",              // 装官方 CLI（自动提取）
		"console.cloud.google.com", // 自建 OAuth 客户端
		gemini.CallbackPath,        // 回调地址必须注册成 /oauth-callback
		"随机端口",                     // 回调端口是动态的，用户得知道要登记 http://localhost
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("文案缺少可操作要素 %q，实际：%s", want, msg)
		}
	}
	// ③ 零 token 纪律：不得把 secret 一类值回显进文案（AGENTS.md §0.2）。
	if strings.Contains(msg, "GOCSPX") {
		t.Errorf("错误文案不得回显凭据：%s", msg)
	}
}

// TestExchangeDialErrorIsClassifiedAsNetwork 守门（2026-10-17）：
// 「Google 不可达」必须与「凭据缺失 / 授权码被拒」**可区分**。
//
// 修复前换码失败直接把 net 错误原样返回（`dial tcp …: i/o timeout`），
// app 侧只把它当成一条普通 err 进 login_error，用户看到的是原始系统报文，
// 既不知道是「网络问题」也不知道该做什么（本机 Google 全线不可达正是这种场景）。
func TestExchangeDialErrorIsClassifiedAsNetwork(t *testing.T) {
	// 取一个有监听者才能连的端口，然后立刻关掉 ⇒ 后续 dial 必定失败（稳定复现「不可达」）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败：%v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	withEndpoints(t, fmt.Sprintf("http://127.0.0.1:%d/token", port), "")

	st := state{RedirectURI: "http://localhost:1234" + gemini.CallbackPath}
	_, terminal, err := exchange(NewClient(), st, "CODE")
	if err == nil {
		t.Fatal("连不上令牌端点时必须报错")
	}
	if !errors.Is(err, ErrGoogleUnreachable) {
		t.Fatalf("dial 失败必须归为 ErrGoogleUnreachable（供 app 区分提示），得 %v", err)
	}
	// 传输层失败不算终态：授权码可能还没被消费，Poll 应继续轮询。
	if terminal {
		t.Error("传输层失败不得判为终态（否则一次网络抖动就废掉整次登录）")
	}
	if !strings.Contains(err.Error(), "oauth2.googleapis.com") {
		t.Errorf("文案应点名不可达的主机，实际：%v", err)
	}
}

// TestOAuthHTTPErrorIsNotNetworkError 边界：端点**可达**但回 4xx 时，
// 不得被误判成网络错误（两者的处置完全不同：一个是换网络，一个是授权码/客户端配置问题）。
func TestOAuthHTTPErrorIsNotNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Bad Request"}`)
	}))
	defer srv.Close()
	withEndpoints(t, srv.URL, "")

	st := state{RedirectURI: "http://localhost:1234" + gemini.CallbackPath}
	_, terminal, err := exchange(NewClient(), st, "CODE")
	if err == nil {
		t.Fatal("4xx 必须报错")
	}
	if errors.Is(err, ErrGoogleUnreachable) {
		t.Fatalf("HTTP 4xx 不是网络不可达，不得归为 ErrGoogleUnreachable：%v", err)
	}
	if !terminal {
		t.Error("拿到 HTTP 响应即属终态")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("应透出 OAuth 信封字段：%v", err)
	}
}
