#!/usr/bin/env bash
# build.sh — 构建 workbuddy-wild.exe，自动备份/恢复用户数据（auths、config、state）。
# 用法：bash build.sh [wails build args...]
#
# 原理：
#   wails build -clean 会删 build/bin/ 全部内容，导致：
#   - auths/（账号凭据）丢失 → 需重新登录
#   - config.json 丢失 → 面板配置重置
#   - data/state-*.json（冷却/签到等运行时状态）丢失
#   本脚本在 -clean 前备份上述文件，构建完成后恢复。
set -euo pipefail

BIN="build/bin"
BACKUP="build/.bin-backup"
WAILS="E:/data/go/bin/wails.exe"

# 1. 清理旧备份
rm -rf "$BACKUP"

# 2. 备份用户数据（仅退出存在时）
if [ -d "$BIN" ]; then
    mkdir -p "$BACKUP"
    # config.json
    [ -f "$BIN/config.json" ] && cp "$BIN/config.json" "$BACKUP/"
    # auths/
    [ -d "$BIN/auths" ] && [ -n "$(ls -A "$BIN/auths" 2>/dev/null)" ] && cp -r "$BIN/auths" "$BACKUP/"
    # data/ 下除 webview 缓存和 app.log 外的文件（如 state-*.json）
    if [ -d "$BIN/data" ]; then
        mkdir -p "$BACKUP/data"
        for f in "$BIN/data"/*.json "$BIN/data"/*.state; do
            [ -f "$f" ] && cp "$f" "$BACKUP/data/"
        done
    fi
    echo "备份用户数据: $(find "$BACKUP" -type f | wc -l) files"
fi

# 3. 图标由 genicon.sh 单独生成；构建时只校验资产，避免每次覆盖图标。
#    icon.png 变更后先执行：bash genicon.sh
for icon in build/appicon.png build/trayicon.ico build/windows/icon.ico; do
    if [ ! -f "$icon" ]; then
        echo "缺少图标资产: $icon，请先执行 bash genicon.sh" >&2
        exit 1
    fi
done
if ! cmp -s icon.png build/appicon.png; then
    echo "build/appicon.png 不是当前 icon.png 生成，请先执行 bash genicon.sh" >&2
    exit 1
fi

# 4. 杀旧进程（如果有）
OLD_PID=$(tasklist //NH //FI "IMAGENAME eq workbuddy-wild.exe" 2>/dev/null | grep -i workbuddy | awk '{print $2}' || true)
if [ -n "$OLD_PID" ]; then
    echo "关闭旧进程 PID=$OLD_PID..."
    taskkill //F //PID "$OLD_PID" 2>/dev/null || true
    sleep 1
fi

# 5. wails build
echo "=== wails build ==="
"$WAILS" build -platform windows/amd64 -clean -skipbindings "$@"

# 6. 恢复备份
if [ -d "$BACKUP" ]; then
    # config.json
    [ -f "$BACKUP/config.json" ] && cp "$BACKUP/config.json" "$BIN/"
    # auths/
    if [ -d "$BACKUP/auths" ] && [ -n "$(ls -A "$BACKUP/auths" 2>/dev/null)" ]; then
        mkdir -p "$BIN/auths"
        cp -r "$BACKUP/auths/"* "$BIN/auths/"
    fi
    # data/
    if [ -d "$BACKUP/data" ]; then
        mkdir -p "$BIN/data"
        for f in "$BACKUP/data"/*; do
            [ -f "$f" ] && cp "$f" "$BIN/data/"
        done
    fi
    echo "恢复用户数据: $(find "$BIN" -type f | grep -v workbuddy-wild.exe | grep -v app\.log | grep -v webview | wc -l) files"
    rm -rf "$BACKUP"
fi

echo "=== 构建完成: $BIN/workbuddy-wild.exe ==="