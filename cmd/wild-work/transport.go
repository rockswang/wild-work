// 出站链路的统一接线：devdump 开发期落盘包装 + 备用上游通道生效。
// 独立成文件避免 main.go 膨胀。
//
// **这里不再有渠道代理逻辑**（决议 R57）：旧的「按渠道套代理 Transport」已删除
// ——启动表 13 个渠道、面板热更新表只 10 个，traecode 两张表都没进（实证静默失效）。
// 备用上游通道改为**零接线**：地址只存进 internal/backuplink，渠道在命中地区拒绝时
// 自行取用（当前唯一取用方 cline），因此不存在「漏接线」这种失效形态。
// 漏项的本表只影响 devdump 落盘范围，由 transport_test.go 静态校验兜住。
package main

import (
	"log"
	"net/http"

	"wild-work/internal/backuplink"
	"wild-work/internal/config"
	"wild-work/internal/devdump"
)

// applyBackupLink 把配置里的备用上游通道装进 backuplink（空串 = 关闭，一切直连）。
// 地址不合法只记日志不中断启动 —— 备用通道是可选备份链路，配错不该把 daemon 拉死，
// 后果只是「命中地区拒绝时原样透传失败」（与未配置同形）。
// 同时把旧 proxies 段的迁移结论打出来（用户配过的代理不能无声消失）。
func applyBackupLink(cfg *config.Config) {
	if note := cfg.LegacyProxyNote; note != "" {
		log.Printf("备用上游通道：%s", note)
	}
	if err := backuplink.Set(cfg.BackupProxy); err != nil {
		log.Printf("备用上游通道配置无效（视为未配置，命中地区拒绝将直接透传）：%v", err)
		return
	}
	if cfg.BackupProxy == "" {
		log.Printf("备用上游通道未配置：所有上游请求直连")
		return
	}
	log.Printf("备用上游通道已就绪：%s（仅渠道命中地区拒绝时按需取用，不参与常规请求）", cfg.BackupProxy)
}

// wrapDumps 给各渠道全部出站 client 套上 devdump（开发期上游原文落盘，见 AGENTS.md §8.5）。
// 发布版 devdump.Wrap 是原样返回的常量实现（构建标签剔除），无任何开销。
// clients 里可含 nil 项（部分渠道没有 BillingHTTP）。
func wrapDumps(targets map[string][]*http.Client) {
	for kind, clients := range targets {
		for _, c := range clients {
			devdump.Wrap(c, kind)
		}
	}
}
