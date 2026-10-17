// load_codearts.go 华为云 CodeArts Agent 渠道的凭据加载器（**新文件**，不改 auth.go 既有函数）。
//
// 本渠道的凭据是华为云 **AK/SK + SecurityToken**（不是 OAuth Bearer）：落盘时
//   - accessToken 承载 AK（账号唯一标识，也是 auth.Parse 非空校验的落点）；
//   - refreshToken 承载 OAuth refresh_token（静默续期用）；
//   - expiresAt 是**临时凭据**的到期时刻（Unix **秒**，与 auth.Auth 全局口径一致）；
//   - 其余（secret_access_key / security_token / code_verifier / dpop_private_key_jwk /
//     domain_id / user_id / user_name / project_id / region / maas_type）都在 auth.Extra。
//
// glob 边界：`codearts-*.json` 里的字面量 `-` 保证既不会被别的渠道吞，也不吞别人——
//   - 现存前缀（workbuddy* / workbuddyai- / trae- / qoder* / qodercn- / qodercom- /
//     qwenwork- / glm- / monkeycode- / raccoon- / loomy- / cline- / officeace- /
//     minimax- / lobsterai-）没有一个以 `codearts` 开头；注意 `cline-*.json` 与
//     `codearts-*.json` 虽同以 `c` 开头，但两者无包含关系（`cli` vs `cod`）；
//   - 反向同样安全：`codearts-*` 要求紧跟一个 `-`，将来若出现 `codearts2-*.json`
//     之类的宽前缀渠道，**不会**被本加载器误收。
//
// 刻意**不**调 AdoptJWTExpiry：AK/SK 不是 JWT（没有 `exp` 可读），到期时刻只能由
// 登录 / 续期的 `expiration`（ISO8601）写入；调它虽是无害空操作，却会误导后来者
// 以为这里另有 JWT 兜底（与 LoadMinimaxDir 同款理由）。
package auth

import (
	"os"
	"path/filepath"
)

// LoadCodeArtsDir 扫描 CodeArts 渠道凭证（codearts-*.json）。
//
// 解析失败或不可读的文件静默跳过（启动日志由调用方统计），Kind 固定为 "codearts"，
// FilePath 指向来源文件（供 refresh 后 SaveAtomic 写回 —— 不变量 20：续期会轮换
// refresh_token 与临时凭据，不落盘 = 下次启动用旧值）。
func LoadCodeArtsDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "codearts-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.Kind, a.FilePath = "codearts", f
		out = append(out, a)
	}
	return out, nil
}
