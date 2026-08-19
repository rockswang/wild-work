// signin 一次性批量签到工具：遍历 ./auths/workbuddy-*.json 与 ./auths/trae-*.json，
// 自动 RefreshToken（过期时），逐个签到，顺手查余额。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/traework"
	"github.com/rockswang/workbuddy-wild/internal/upstream"
)

type checkinClient interface {
	RefreshToken(*auth.Auth) error
	DailyCheckin(*auth.Auth) error
	UserResource(*auth.Auth) (int64, error)
}

type row struct {
	file     string
	platform string
	uid      string
	nick     string
	status   string // OK | ALREADY | FAIL | AUTH_INVALID | LOAD_ERR
	detail   string
	remain   int64
	hasQuota bool
}

func main() {
	dir := "auths"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	files := make([]string, 0)
	for _, pattern := range []string{"workbuddy-*.json", "trae-*.json"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			continue
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", dir)
		os.Exit(1)
	}
	sort.Strings(files)

	var rows []row
	okN, alreadyN, failN := 0, 0, 0
	for _, f := range files {
		platform := "workbuddy"
		var up checkinClient = upstream.New()
		if strings.HasPrefix(filepath.Base(f), "trae-") {
			platform = "traework"
			up = traework.New()
		}
		r := row{file: filepath.Base(f), platform: platform}
		raw, err := os.ReadFile(f)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a.FilePath = f
		r.uid, r.nick = a.UID, a.Nickname

		// 参考 traework2api：签到前 2 小时内过期就先刷新，避免签到请求带旧 token。
		if a.NeedsRefresh(2 * time.Hour) {
			if err := up.RefreshToken(a); err != nil {
				r.status = "FAIL"
				if isAuthInvalid(err) {
					r.status = "AUTH_INVALID"
				}
				r.detail = "refresh: " + short(err.Error())
				rows = append(rows, r)
				failN++
				continue
			}
			if err := a.SaveAtomic(); err != nil {
				// 本次使用的是内存中的新 token；落盘失败只记录，不阻止签到。
				r.detail = "refresh save: " + short(err.Error())
			}
		}

		if err := up.DailyCheckin(a); err == nil {
			r.status = "OK"
			okN++
		} else if isAlready(err) {
			r.status, r.detail = "ALREADY", short(err.Error())
			alreadyN++
		} else {
			r.status, r.detail = "FAIL", short(err.Error())
			failN++
		}
		if remain, qerr := up.UserResource(a); qerr == nil {
			r.remain, r.hasQuota = remain, true
		}
		rows = append(rows, r)
	}

	fmt.Printf("platform   | uid                                  | nick        | status       | remain | detail\n")
	fmt.Printf("-----------+--------------------------------------+-------------+--------------+--------+------------------------------\n")
	for _, r := range rows {
		remain := "-"
		if r.hasQuota {
			remain = fmt.Sprintf("%d", r.remain)
		}
		fmt.Printf("%-10s | %-36s | %-11s | %-12s | %-6s | %s\n",
			r.platform, trunc(r.uid, 36), trunc(r.nick, 11), r.status, remain, r.detail)
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d fail=%d\n", len(rows), okN, alreadyN, failN)
}

func isAlready(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already check") ||
		strings.Contains(s, "already checked")
}

func isAuthInvalid(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "session_dead") || strings.Contains(s, "unauthorized") || strings.Contains(s, "http 401")
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func short(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		return s[:60]
	}
	return s
}
