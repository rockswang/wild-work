#!/usr/bin/env bash
# 把 macOS 二进制打包成可双击的 WildWork.app，并生成 .dmg 安装包。
#
# 背景（数据目录如何解决）：
#   cmd/wild-work 启动时会 os.Chdir 到「可执行文件所在目录」，并把
#   config.json / auths/ / data/ 都写在该目录（保证相对路径稳定）。
#   .app 装进 /Applications 后包内容对普通用户只读，直接放二进制会写失败。
#   解决办法在程序侧：main.go 的 workDir() 检测到自身位于
#   .app/Contents/MacOS 内时，自动把数据目录切到
#   ~/Library/Application Support/WildWork。
#
#   ⚠️ 不要改回「用 shell 启动器脚本包一层」的老做法：macOS 要求 .app 的
#   CFBundleExecutable 是真正的 Mach-O 主可执行文件，脚本再 exec 进真实
#   二进制会导致 AppKit 建不出 NSStatusItem —— 守护进程正常跑、菜单栏却
#   没有图标（已实测复现，见下表）。所以这里必须直接把二进制放进 MacOS/。
#
# 用法：
#   build/macos/build-app.sh                    # 自动找 dist/wild-work-darwin-<arch>
#   BIN=dist/wild-work-darwin-arm64-2-2-1 build/macos/build-app.sh
#   ARCH=amd64 build/macos/build-app.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

ARCH="${ARCH:-$(uname -m)}"
VERSION="$(grep -oE 'const Version = "[^"]+"' internal/app/app.go | head -1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')"
APP_NAME="WildWork"
BUNDLE_ID="com.rockswang.wild-work"

mkdir -p dist

# 定位二进制：优先 BIN 显式指定，其次规范名，最后带版本号的名字
if [[ -n "${BIN:-}" && -f "$BIN" ]]; then
  SRC_BIN="$BIN"
elif [[ -f "dist/wild-work-darwin-$ARCH" ]]; then
  SRC_BIN="dist/wild-work-darwin-$ARCH"
elif [[ -f "dist/wild-work-darwin-$ARCH-$VERSION" ]]; then
  SRC_BIN="dist/wild-work-darwin-$ARCH-$VERSION"
else
  echo "找不到 $ARCH 的二进制。请先构建：" >&2
  echo "  GOOS=darwin GOARCH=$ARCH CGO_ENABLED=1 go build -tags devdump -trimpath -o dist/wild-work-darwin-$ARCH ./cmd/wild-work" >&2
  exit 1
fi

echo "==> 版本 $VERSION / 架构 $ARCH"
echo "==> 源二进制 $SRC_BIN"

# ---------------------------------------------------------------- 图标
# 生成到 dist/（构建产物，不入版本控制），从仓库内已有的 build/appicon.png 派生。
ICNS="dist/AppIcon.icns"
if [[ ! -f "$ICNS" ]]; then
  echo "==> 生成图标 $ICNS"
  SRC_ICON="build/appicon.png"
  [[ -f "$SRC_ICON" ]] || { echo "缺少 $SRC_ICON" >&2; exit 1; }
  ICONSET="dist/AppIcon.iconset"
  rm -rf "$ICONSET"; mkdir -p "$ICONSET"
  for spec in "16:icon_16x16.png" "32:icon_16x16@2x.png" "32:icon_32x32.png" \
              "64:icon_32x32@2x.png" "128:icon_128x128.png" "256:icon_128x128@2x.png" \
              "256:icon_256x256.png" "512:icon_256x256@2x.png" "512:icon_512x512.png" \
              "1024:icon_512x512@2x.png"; do
    px="${spec%%:*}"; out="${spec##*:}"
    sips -s format png -z "$px" "$px" "$SRC_ICON" --out "$ICONSET/$out" >/dev/null
  done
  iconutil -c icns "$ICONSET" -o "$ICNS"
  rm -rf "$ICONSET"
fi

# ---------------------------------------------------------------- .app
APP="dist/$APP_NAME.app"
echo "==> 组装 $APP"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

cp "$SRC_BIN" "$APP/Contents/MacOS/$APP_NAME"
chmod 755 "$APP/Contents/MacOS/$APP_NAME"
cp "$ICNS" "$APP/Contents/Resources/AppIcon.icns"
printf 'APPL????' > "$APP/Contents/PkgInfo"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>$APP_NAME</string>
	<key>CFBundleDisplayName</key><string>$APP_NAME</string>
	<key>CFBundleExecutable</key><string>$APP_NAME</string>
	<key>CFBundleIdentifier</key><string>$BUNDLE_ID</string>
	<key>CFBundleIconFile</key><string>AppIcon</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
	<key>CFBundleShortVersionString</key><string>$VERSION</string>
	<key>CFBundleVersion</key><string>$VERSION</string>
	<key>LSMinimumSystemVersion</key><string>11.0</string>
	<!-- 托盘常驻程序：不在 Dock 显示图标 -->
	<key>LSUIElement</key><true/>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

# 注意：这里**不能**用 shell 启动器脚本包一层。
# macOS 要求 .app 的 CFBundleExecutable 是真正的 Mach-O 主可执行文件；
# 若用 bash 脚本再 exec 进真实二进制，AppKit 无法建立 NSStatusItem，
# 表现为「守护进程正常运行、菜单栏却没有任何图标」（已实测复现）。
# 数据目录改由程序自身处理：cmd/wild-work 的 workDir() 在检测到
# 自身位于 .app/Contents/MacOS 内时，自动改用
# ~/Library/Application Support/WildWork，因此 .app 可以整体只读。

chmod 755 "$APP/Contents/MacOS/$APP_NAME"

# ---------------------------------------------------------------- 签名
# arm64 二进制必须至少有 ad-hoc 签名才能执行；内层先签，再签整个 bundle。
echo "==> ad-hoc 签名"
codesign --force --sign - --timestamp=none "$APP/Contents/MacOS/$APP_NAME" 2>/dev/null
codesign --force --sign - --timestamp=none "$APP"
codesign --verify --deep --strict "$APP" && echo "    签名校验通过"

# ---------------------------------------------------------------- .dmg
# hdiutil 需要访问磁盘设备节点；在受限/沙箱环境会失败。
#
# 失败只告警，不影响 .app。注意：先写到临时文件、成功后才 mv 覆盖，
# 否则一旦创建失败就会把上一次已生成好的 dmg 删掉（曾经的 bug）。
DMG="dist/$APP_NAME-$VERSION-$ARCH.dmg"
# 临时名必须以 .dmg 结尾：hdiutil 会给不含该后缀的目标自动补上 ".dmg"，
# 若用 "$DMG.tmp" 会实际产出 "$DMG.tmp.dmg"，导致下面的 mv 找不到源文件。
DMG_TMP="${DMG%.dmg}.tmp.dmg"
echo "==> 生成 $DMG"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
cp -R "$APP" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
rm -f "$DMG_TMP"
DMG_OK=1
if hdiutil create -volname "$APP_NAME" -srcfolder "$STAGE" -ov -format UDZO -quiet "$DMG_TMP"; then
  mv -f "$DMG_TMP" "$DMG"
else
  DMG_OK=0
  rm -f "$DMG_TMP"
  echo "    !! DMG 生成失败（hdiutil 需磁盘设备权限，沙箱/CI 受限环境下常见）" >&2
  if [[ -f "$DMG" ]]; then
    echo "       已保留原有 dmg：${DMG}（未被覆盖）。" >&2
  else
    echo "       .app 已生成，可直接使用；如需 DMG 请在普通终端重跑本脚本。" >&2
  fi
  # CI 必须拿到 dmg：REQUIRE_DMG=1 时生成失败即报错退出，避免静默发出不含 dmg 的 release。
  if [[ "${REQUIRE_DMG:-0}" == "1" ]]; then
    echo "       REQUIRE_DMG=1，视为致命错误。" >&2
    exit 1
  fi
fi

echo
echo "完成："
echo "  $APP"
if [[ "$DMG_OK" == 1 ]]; then
  echo "  $DMG"
  du -sh "$APP" "$DMG" | sed 's/^/  /'
else
  du -sh "$APP" | sed 's/^/  /'
fi
