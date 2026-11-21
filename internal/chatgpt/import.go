package chatgpt

// import.go 负责「从 ww3 客户端数据目录里找出候选凭据文件」这一件事（票 08）。
//
// 只做**文件定位**，不解析、不校验：解析必须复用 cred.go 的 ParseAuth（与票 01 同源，
// 另写一份必然漂移）；校验与落盘属于 internal/app 的编排（chatgpt_import.go）。
//
// 路径口径刻意宽容，因为用户手里可能是指向数据目录、auths/ 目录本身，或单个文件：
//   - 目录下有 auths/ 子目录 ⇒ 扫 auths/*.json（ww3 客户端的真实布局）；
//   - 否则扫该目录下的 *.json（用户直接指了 auths/ 目录，或未来布局变了）；
//   - 指向文件 ⇒ 只试这一个。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ScanAuthFiles 返回候选凭据文件的绝对路径（按文件名升序，结果稳定可复现）。
//
// 返回错误只覆盖「路径这一层」的问题（不存在 / 不是文件或目录 / 目录里没有 .json），
// 目的就是票面锚点要求的「让用户看得出是**路径不对**」；单个文件内容的好坏交给
// 调用方逐条回传（那是「凭据过期/不是凭据」的另一类问题）。
func ScanAuthFiles(path string) ([]string, error) {
	p, err := normalizeImportPath(path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("路径不存在：%s（请填 ww3 客户端的数据目录，通常包含 auths/ 子目录）", p)
		}
		return nil, fmt.Errorf("无法访问路径 %s：%w", p, err)
	}
	if fi.IsDir() {
		return scanDir(p)
	}
	return []string{p}, nil
}

// normalizeImportPath 去掉常见粘贴噪声（首尾空白、成对引号），并展开开头的 `~`。
//
// 不做更激进的猜测（例如把 Windows 盘符路径在 Linux 上改写）：路径是用户亲手指的，
// 猜错只会把「路径不对」变成「读了别的目录」。
func normalizeImportPath(path string) (string, error) {
	p := strings.TrimSpace(path)
	p = strings.Trim(p, "\"'")
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("请填写 ww3 客户端的数据目录（通常包含 auths/ 子目录）")
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			rest := strings.TrimLeft(strings.TrimPrefix(p, "~"), `/\`)
			p = home
			if rest != "" {
				p = filepath.Join(home, rest)
			}
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("无法解析路径 %s：%w", p, err)
	}
	return abs, nil
}

// scanDir 列出目录下的候选凭据文件；优先 auths/ 子目录。
func scanDir(dir string) ([]string, error) {
	target := dir
	if sub := filepath.Join(dir, "auths"); isDir(sub) {
		target = sub
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败 %s：%w", target, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") { // .DS_Store / 编辑器临时文件
			continue
		}
		if !strings.EqualFold(filepath.Ext(name), ".json") {
			continue
		}
		out = append(out, filepath.Join(target, name))
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("目录里没有找到凭据文件：%s（期望 auths/ 子目录或本目录下有 *.json）", dir)
	}
	return out, nil
}

// isDir 报告路径是否是已存在的目录。
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
