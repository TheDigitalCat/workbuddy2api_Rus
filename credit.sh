#!/usr/bin/env bash
# credit.sh — суточный отчёт по баллам WorkBuddy (по умолчанию красивый вывод)
#
# Использование:
#   ./credit.sh            # человекочитаемый отчёт
#   ./credit.sh -json      # сырой JSON
#
# Обновление бинарника: go build -o credit ./cmd/credit
set -euo pipefail
cd "$(dirname "$0")"
if [[ "${1:-}" == "-json" ]]; then
    exec ./credit
fi
exec ./credit -pretty
