package auth

// LoadGeminiDir 扫描 Gemini（Google Code Assist / Cloud Code）渠道凭证（gemini-*.json）。
//
// 与 LoadMinimaxDir / LoadLobsterAIDir 同口径：解析失败或不可读的文件静默跳过
// （启动日志由调用方统计），Kind 固定为 "gemini"，FilePath 指向来源文件
// （供 refresh 后 SaveAtomic 写回 —— 不变量 20：refresh token 轮换后必须落盘）。
//
// glob 边界（AGENTS.md §3「新增渠道步骤」第 ② 条专门提醒过这类吞并）：
//   - `gemini-*.json` 里的字面量 `-` 保证既不会被别的渠道吞，也不吞别人——
//     现存前缀（workbuddy* / workbuddyai- / trae- / qoder* / qodercn- / qodercom- /
//     qwenwork- / glm- / monkeycode- / raccoon- / loomy- / cline- / officeace- /
//     minimax- / lobsterai-）没有一个以 `gemini` 开头，故本 glob 不会命中它们的文件；
//   - 反向同样安全：`gemini-*` 要求紧跟一个 `-`，所以将来若出现 `gemini2-*.json`
//     之类的宽前缀渠道，**不会**被本加载器误收（回归测试已锁死这一点）。
//
// 刻意**不**调 AdoptJWTExpiry：Google 的 access token 是 opaque token（`ya29.` 开头，
// 不是 JWT），没有 `exp` 可读（实测 0 个点）。ExpiresAt 只能由登录 / 续期时的
// `now + expires_in`（**单位秒**）写入。调它虽然是无害空操作（解析不出 exp 即返回 0），
// 但会误导后来者以为这里有 JWT 兜底。
func LoadGeminiDir(dir string) ([]*Auth, error) {
	return loadPrefixed(dir, "gemini")
}
