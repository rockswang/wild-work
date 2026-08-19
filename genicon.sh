#!/usr/bin/env bash
# genicon.sh — 仅在 icon.png 变更后执行一次，生成 Wails/exe/托盘图标资产。
set -euo pipefail
cd "$(dirname "$0")"
go run ./cmd/genicon
