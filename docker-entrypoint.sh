#!/bin/sh
# 检测可用浏览器并设置 ROD_BROWSER_BIN
if [ -f /usr/bin/google-chrome ]; then
    export ROD_BROWSER_BIN=/usr/bin/google-chrome
elif [ -f /usr/bin/chromium ]; then
    export ROD_BROWSER_BIN=/usr/bin/chromium
fi
exec "$@"
