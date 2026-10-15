#!/usr/bin/env bash
# probe-dump.sh 开发期「缓存利用率实地取证」探针。
#
# 做什么：对每个渠道各发 1~2 次真实对话请求（强制 stream=false 以拿**单包 usage**，
# 避免在 SSE 里翻末帧），把上游原始响应 dump 到 $WILDWORK_DUMP_DIR。
# 随后可用 summarize 子命令汇总每个渠道的 usage 字段，一眼看出谁有缓存数据。
#
# 前置条件：
#   1. daemon 由 **devdump 构建** 启动，且环境变量 WILDWORK_DUMP_DIR 已设置：
#        export WILDWORK_DUMP_DIR="$PWD/temp/dump"
#        go build -tags devdump -o dist/wild-work-devdump.exe ./cmd/wild-work
#        WILDWORK_DUMP_DIR="$PWD/temp/dump" ./dist/wild-work-devdump.exe --no-tray
#      （普通 dist/wild-work.exe 无 dump 能力：发布版该开关恒为惰性）
#   2. daemon 已加载各渠道账号（auths/ 目录）。
#
# 用法：
#   scripts/probe-dump.sh summarize      # 汇总已有 dump（不发起请求）
#   scripts/probe-dump.sh run            # 逐渠道发起对话
#   scripts/probe-dump.sh run --only workbuddy,glm
#
# 注意：会真实消耗各账号额度（每次约几十 token）。
set -uo pipefail

HOST="${WILDWORK_HOST:-http://127.0.0.1:7863}"
API_KEY="${WILDWORK_API_KEY:-WildWorkAPI}"
DUMP_DIR="${WILDWORK_DUMP_DIR:-$PWD/temp/dump}"
OUT="$PWD/temp/probe"
mkdir -p "$OUT"

# ch 模型 prompt：每渠道一个「便宜 + 存在」的模型（由 /v1/models 实际下发为准）。
# 模型名可在运行时被 --model-<ch> 覆盖，或直接改此表。
PROBES=(
  "workbuddy|workbuddy/glm-5.3-flash"
  "workbuddyai|workbuddyai/glm-5.3"
  "traework|traework/DeepSeek-V4-Flash"
  "traecode|traecode/DeepSeek-V4-Flash"
  "qodercn|qodercn/glm-5.3-flash"
  "qodercom|qodercom/qwen3.8-flash"
  "qwenwork|qwenwork/flash"
  "monkeycode|monkeycode/basic/deepseek-flash"
  "raccoon|raccoon/sn-deepseek-v4-1-flash"
  "loomy|loomy/DeepSeek-V4.1-Flash"
  "glm|glm/chatglm:moe_53f"
  "cline|cline/cline-free/mimo-v2.6-flash"
  "oczen|oczen/big-pickle"
)

# 每渠道两轮：首轮是冷前缀（cache_read≈0），次轮重复同一大前缀（应命中缓存）。
# 这样 dump 里能同时看到「未命中」与「命中」两种 usage 形状。
PROMPT_PREFIX="缓存命中探针：以下是同一段重复前缀，用于观察上游是否回报缓存字段。$(printf 'X%.0s' {1..400})"

post_chat() { # ch model stream prompt
  local ch="$1" model="$2" stream="$3" prompt="$4"
  local payload
  payload=$(cat <<JSON
{"model":"$model","stream":$stream,"messages":[{"role":"user","content":$(_json_str "$prompt")}]}
JSON
)
  curl.exe -sS -m 180 "$HOST/v1/chat/completions" \
    -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
    -d "$payload" -o "$OUT/$ch-$stream.json" -w '%{http_code}' 2>"$OUT/$ch-$stream.err"
}

_json_str() { # 用 jq 安全编码字符串（避免引号/中文破坏 JSON）
  printf '%s' "$1" | D:/dev/Git/cmd/jq.exe -Rs .
}

summarize() {
  echo "dump 根目录：$DUMP_DIR"
  [ -d "$DUMP_DIR" ] || { echo "（不存在——daemon 未开启 dump，或还没发起过对话）"; return; }
  printf '\n%-12s %-6s %s\n' CHANNEL FILES "usage 字段（末帧）"
  printf '%s\n' "--------------------------------------------------------------------------"
  for d in "$DUMP_DIR"/*/; do
    [ -d "$d" ] || continue
    ch=$(basename "$d")
    n=$(find "$d" -name '*.http' | wc -l)
    # 取最新一份快照的 "# usage=" 行
    latest=$(ls -t "$d"*.http 2>/dev/null | head -1)
    hint="(no snapshot)"
    [ -n "$latest" ] && hint=$(grep -am1 '^# usage=' "$latest" | sed 's/^# usage=//')
    printf '%-12s %-6s %s\n' "$ch" "$n" "$hint"
  done
  echo
  echo "带缓存字段的渠道（cached_tokens / cache_read / prompt_cache_hit）："
  grep -raho 'prompt_cache_hit_tokens"[^,}]*\|cached_tokens"[^,}]*\|cache_read_input_tokens"[^,}]*\|cache_creation_input_tokens"[^,}]*' \
    "$DUMP_DIR" 2>/dev/null | sort | uniq -c | sort -rn | head -30
}

run_probes() {
  local only="${1:-}"
  echo "目标 daemon：$HOST"
  code=$(curl.exe -sS -m 10 -o /dev/null -w '%{http_code}' "$HOST/v1/models" -H "Authorization: Bearer $API_KEY")
  if [ "$code" != "200" ]; then echo "daemon 不可达（HTTP $code）——先启动 daemon"; exit 1; fi

  for entry in "${PROBES[@]}"; do
    ch="${entry%%|*}"; model="${entry##*|}"
    if [ -n "$only" ] && [[ ",$only," != *",$ch,"* ]]; then continue; fi
    echo "== $ch  ($model)"
    printf '   冷前缀: HTTP %s\n' "$(post_chat "$ch" "$model" false "$PROMPT_PREFIX 第一问")"
    printf '   热前缀: HTTP %s\n' "$(post_chat "$ch" "$model" false "$PROMPT_PREFIX 第二问")"
  done
  echo "完成。用 scripts/probe-dump.sh summarize 汇总。"
}

case "${1:-summarize}" in
  summarize) summarize ;;
  run) run_probes "${2:-}" ;;
  *) echo "用法：$0 [summarize|run [--only ch1,ch2]]"; exit 2 ;;
esac
