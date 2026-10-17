package auth

import (
	"os"
	"path/filepath"
)

// LoadZCodeDir 扫描智谱 ZCode 渠道凭证（`zcode-*.json`）。
//
// 与 LoadMinimaxDir / LoadLobsterAIDir 同口径：解析失败或不可读的文件静默跳过
// （启动日志由调用方统计），Kind 固定为 "zcode"，FilePath 指向来源文件。
//
// glob 边界（新增渠道的固定自查项）：`zcode-*.json` 里的字面量 `-` 保证既不会被别的
// 渠道吞，也不吞别人——
//   - 现存前缀（workbuddy* / workbuddyai- / trae- / qoder* / qodercn- / qodercom- /
//     qwenwork- / glm- / monkeycode- / raccoon- / loomy- / cline- / officeace- /
//     minimax- / lobsterai- / gemini- / codearts-）没有一个以 `zcode` 开头；
//   - 反向同样安全：`zcode-*` 要求紧跟一个 `-`，所以将来若出现 `zcode2-*.json` 之类的
//     宽前缀渠道，**不会**被本加载器误收。
//
// 刻意**不**调 AdoptJWTExpiry：本渠道的 zcode_jwt **没有 `exp`**（协议 §1.8：
// JWT payload 无 exp、不可续期），解不出任何到期时刻，调它只会让后来者以为这里
// 另有 JWT 兜底。ExpiresAt 由登录包写成远期值（否则 NeedsRefresh 恒真 → 反复空转刷新）。
func LoadZCodeDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "zcode-*.json"))
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
		a.Kind, a.FilePath = "zcode", f
		out = append(out, a)
	}
	return out, nil
}
