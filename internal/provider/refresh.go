package provider

import (
	"fmt"
	"strings"
	"sync"

	"wild-work/internal/auth"
)

// ---------------------------------------------------------------------------
// token 刷新单飞（singleflight）
// ---------------------------------------------------------------------------
//
// 为什么必须有这一层：部分渠道的 refresh_token 是**单会话**的 —— 上游每次刷新都会轮换它，
// 旧值立刻作废。同一账号的两次刷新一旦并发发出，两者都握着同一个旧 refresh_token，结果
// 必然是一成功一失败（raccoon 报 `200822 refresh_conflict`，qwenwork 报 invalid_grant），
// 失败那次还会把账号推进冷却（默认 10 分钟）甚至判定 session dead，对外表现为
// 「偶发 503 / 账号莫名不可用」。
//
// 这不是理论风险：阶段 E 验收时 4 个并发请求各自触发刷新 → 1 成功 3 冲突 → 账号冷却；
// 补测时即便**单线程顺序**发请求，后台 credit/pricing 循环与请求路径之间仍会撞车。
//
// auth.Auth 自带的 mu 只保证「不会并发读写字段」，拦不住「两次刷新都真的打上游」——
// 串行执行照样各发一次请求，第二个用的还是已被轮换的旧 token。真正需要的是单飞：
// 同一账号同一时刻只放一次真实刷新出去，其余调用等待并共享这次的结果。
//
// 单飞键用 auth 文件路径（见 refreshKey），跨渠道天然隔离，调用方无需再传渠道标识。

// RefreshOnce 对同一账号的并发刷新做单飞。
//
// fn 内部应当**重新检查一次是否仍需要刷新**，例如：
//
//	provider.RefreshOnce(acct, func() error {
//	    if !acct.NeedsRefresh(skew) {
//	        return nil // 已被并发的另一次刷新刷过
//	    }
//	    if err := upstream.RefreshToken(acct); err != nil {
//	        return err
//	    }
//	    return acct.SaveAtomic()
//	})
//
// 这个内层重检是必要的：等待者醒来时 token 可能已经是新的，重检让它直接返回而不是再打一次
// 上游；同时也封住了「上一轮 flight 刚结束、表项刚删除」这个窗口 —— 后到的调用会在 fn 内
// 自行跳过。
//
// 例外是 401 自愈场景：session 已被上游作废而本地 expiresAt 尚未到期，此时不能靠
// NeedsRefresh 判断，那类 fn 应无条件刷新。
//
// 返回值即本次真实执行的错误，等待者拿到同一个值（含成功）。fn panic 时 defer 会先唤醒
// 所有等待者（附 panic 错误）再把 panic 向上抛，不会让它们永久阻塞。
func RefreshOnce(a *auth.Auth, fn func() error) error {
	return refreshFlights.do(refreshKey(a), fn)
}

// refreshKey 单飞键。
//
// 以 auth 文件路径为准：它天然唯一（各渠道 loader 各扫各的文件名前缀），且「同一账号在两个
// 目录下各有一份文件」本就代表两个独立的 refresh 会话，理应各自单飞 —— 若改用 UID 反而会
// 把它们错误合并成一次刷新。FilePath 为空（账号尚未落盘）时回落到 UID。
func refreshKey(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	if p := strings.TrimSpace(a.FilePath); p != "" {
		return p
	}
	return strings.TrimSpace(a.UID)
}

// flightGroup 是按 key 的单飞组。map + channel 手写，不引入 golang.org/x/sync 依赖
// （本仓 go.mod 刻意保持极小）。
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

// flightCall 一次进行中的刷新。err 在 close(done) 之前写入，close 建立的 happens-before
// 保证等待者读到的一定是最终值。
type flightCall struct {
	done chan struct{}
	err  error
}

var refreshFlights = &flightGroup{calls: make(map[string]*flightCall)}

// do 执行 fn；同一 key 上已有进行中的调用时，等待并共享其结果。
func (g *flightGroup) do(key string, fn func() error) error {
	if key == "" {
		// 既无 FilePath 也无 UID：定位不到账号，无法建 flight 表项。
		// 这类账号本来也落不了盘（SaveAtomic 会报 no FilePath set），直接执行即可。
		return fn()
	}

	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.err
	}
	c := &flightCall{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	// 顺序有讲究：
	//   - 先写 err 再 close，等待者才不会读到零值；
	//   - 先 close 再删表项，close 与 delete 之间进来的调用者会看到已完成的 flight 并直接
	//     复用结果，而不是新开一轮重复打上游（那个窗口极小，但正好落在「刷新刚成功、
	//     并发请求刚拿到 401」的交界上，重复刷新就是一次多余的 refresh_conflict）。
	finish := func(err error) {
		c.err = err
		close(c.done)
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
	}

	defer func() {
		if r := recover(); r != nil {
			finish(fmt.Errorf("refresh panic: %v", r))
			panic(r)
		}
	}()

	err := fn()
	finish(err)
	return err
}
