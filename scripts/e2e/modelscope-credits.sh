#!/usr/bin/env bash
# ModelScope 魔粒额度面的可复跑入口（Go 整机层，非 panel⇄backend 那套 Node 套件）。
#
# 用例在 internal/app/e2e_modelscope_{harness,site_fake,credits}_test.go：
#   对话假上游（api-inference host）+ 主站假上游（modelscope.cn host，**另一台 httptest**）
#   → 真实渠道客户端（Client.Base / Client.SiteBase 注入）→ 真实 TCP 端口 → 真 /api/* 请求。
# 覆盖面：票 02（余额/今日用量/面板口径/失败保快照）、票 03（逐笔到期 FIFO + ΣRemain 对齐）、
#         票 04（每日领取 claimed / already_claimed / no_campaign 三态与文案）。
#
# 为什么这里只有一行 go test：本套件的「整机」全部在进程内（httptest + 真端口），
# 与 scripts/e2e/*.mjs（真 daemon 进程 + 最小 DOM）不同层，不需要另起进程或装 Node 依赖。
# 这条脚本的价值是给一个不用记测试名的入口：默认 -v、-count=1（防缓存假装通过），
# 附加参数原样透传给 go test（例：`bash scripts/e2e/modelscope-credits.sh -run FIFO`）。
#
# 不打真上游：两区凭据都是假 token，两个 host 都是 httptest（AGENTS §0.1）。
set -euo pipefail
cd "$(dirname "$0")/../.."

exec env GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" \
  go test ./internal/app/ -run '^TestE2E_MSCredits' -count=1 -v "$@"