#!/bin/sh
# 若安装了 Google Chrome 则使用，否则 go-rod 自动检测 ARM64 Chromium
if [ -f /usr/bin/google-chrome ]; then
    export ROD_BROWSER_BIN=/usr/bin/google-chrome
fi
exec "$@"
