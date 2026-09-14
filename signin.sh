#!/bin/bash
# Пакетный чек-ин: перебирает все аккаунты workbuddy-*.json в каталоге auths/
# Использование: ./signin.sh [auths_dir]
set -e
cd "$(dirname "$0")"

BIN=./signin_bin
if [ ! -x "$BIN" ]; then
    echo "собираю signin_bin ..."
    go build -o "$BIN" ./cmd/signin
fi

exec "$BIN" "${1:-auths}"
