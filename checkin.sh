#!/usr/bin/env bash
# checkin.sh — вспомогательный инструмент табличного чек-ина (тонкая обёртка)
#
# HTTP-интерфейс чек-ина напрямую не дёргаем: cmd/signin уже сам даёт табличный вывод (uid/nick/status/remain/detail
# + строка итога total=N ok=N already=N fail=N) и предобновление token (NeedsRefresh(2h)),
# это полнее, чем обёртка поверх HTTP. Скрипт:
#   1. читает порт и api_key из config.json, ходит GET /status, чтобы понять, жив ли шлюз
#      (если жив — лишь подсказка «плановый чек-ин уже работает на стороне шлюза»);
#   2. основное действие = собрать и выполнить ./signin_bin auths, прозрачно отдав его табличный вывод;
#   3. в режиме -v вылавливает из таблицы детали fail (FAIL/LOAD_ERR/AUTH_INVALID, «уже отмечено» не считается).
#
# Использование:
#   ./checkin.sh            # заголовок состояния шлюза + таблица signin_bin
#   ./checkin.sh -v         # таблица + сводка деталей fail в конце
#   ./checkin.sh auths_dir  # указать каталог auths (пробрасывается в signin_bin)
#
# Зависимости: go (первая сборка signin_bin), curl, jq (без jq заголовок /status деградирует до сырого JSON)
#
# Переменные окружения:
#   WB2A_CONFIG  путь к файлу конфигурации (по умолчанию ./config.json, оттуда берутся порт и api_key)
#   WB2A_URL     напрямую задать адрес сервиса (напр. http://1.2.3.4:7863), при задании конфиг игнорируется
set -euo pipefail
cd "$(dirname "$0")"

VERBOSE=0
AUTHS_DIR="auths"
# Разбор параметров: -v/-h и опциональный каталог auths. Строгий стиль разбора как в PR #48.
while [[ $# -gt 0 ]]; do
    case "$1" in
        -v|--verbose) VERBOSE=1; shift ;;
        -h|--help)
            sed -n '2,20p' "$0"
            exit 0 ;;
        -*) echo "Неизвестный параметр: $1 (доступны -v / -h)" >&2; exit 2 ;;
        *) AUTHS_DIR="$1"; shift ;;
    esac
done

if ! command -v curl >/dev/null 2>&1; then
    echo "Требуется curl" >&2
    exit 1
fi
HAS_JQ=1
command -v jq >/dev/null 2>&1 || HAS_JQ=0

# ─── Разбор адреса сервиса и ключа: приоритет WB2A_URL, затем config.json ──────────────────────
CFG=${WB2A_CONFIG:-config.json}
PORT=7863
KEY=""
if [[ -f "$CFG" ]] && [[ "$HAS_JQ" == "1" ]]; then
    LISTEN=$(jq -r '.listen // ":7863"' "$CFG")
    PORT=${LISTEN##*:}
    KEY=$(jq -r '.api_key // ""' "$CFG")
fi
PORT=${PORT:-7863}
BASE=${WB2A_URL:-http://localhost:$PORT}

AUTH=()
[[ -n "$KEY" ]] && AUTH=(-H "Authorization: Bearer $KEY")

# ─── Заголовок состояния шлюза: GET /status проверяет живость (жив = плановый чек-ин уже работает на стороне шлюза) ───────
# -s — тихо, -w — отдельно взять код состояния; обрыв соединения глушим через || true, CODE сводим к 000.
GATEWAY_OK=0
GATEWAY_INFO=""
RESP=$(curl -s -w $'\n%{http_code}' ${AUTH+"${AUTH[@]}"} "$BASE/status" || true)
if [[ -n "$RESP" ]]; then
    CODE=${RESP##*$'\n'}
    BODY=${RESP%$'\n'*}
    if [[ "$CODE" == "200" ]]; then
        GATEWAY_OK=1
        if [[ "$HAS_JQ" == "1" ]]; then
            GATEWAY_INFO=$(printf '%s' "$BODY" | jq -r \
                '"Шлюз онлайн: total=\(.total) healthy=\(.healthy) cooling=\(.cooling) disabled=\(.disabled)"' 2>/dev/null || true)
        fi
        [[ -z "$GATEWAY_INFO" ]] && GATEWAY_INFO="Шлюз онлайн (/status 200)"
    else
        case "$CODE" in
            401) GATEWAY_INFO="Ошибка авторизации шлюза (401): api_key не совпадает с config.json" ;;
            000) GATEWAY_INFO="Нет соединения с $BASE: сервис не запущен или неверный порт" ;;
            *)   GATEWAY_INFO="Аномальный /status шлюза (HTTP $CODE)" ;;
        esac
    fi
else
    GATEWAY_INFO="Нет соединения с $BASE: сбой curl"
fi

echo "── Состояние шлюза ─────────────────────────────"
echo "$GATEWAY_INFO"
if [[ "$GATEWAY_OK" == "1" ]]; then
    echo "Подсказка: плановый чек-ин уже работает на стороне шлюза (09/21 ч). Ниже — офлайн-инструмент для мгновенной сверки."
fi
echo

# ─── Ядро: собрать и выполнить signin_bin (переиспользуем логику сборки из signin.sh) ────────────────
BIN=./signin_bin
if [[ ! -x "$BIN" ]]; then
    echo "собираю signin_bin ..."
    if ! command -v go >/dev/null 2>&1; then
        echo "Нужен go для сборки signin_bin (или сначала соберите через ./signin.sh)" >&2
        exit 1
    fi
    go build -o "$BIN" ./cmd/signin
fi

# signin_bin уже даёт табличный вывод + строку итога, отдаём как есть. В -v сначала пишем в файл, затем цепляем детали fail в конец.
if [[ "$VERBOSE" == "1" ]]; then
    OUT=$("$BIN" "$AUTHS_DIR") || true
    printf '%s\n' "$OUT"
    echo
    echo "── Детали проблем (только fail/ошибки, «уже отмечено» не считается) ──"
    # Строки таблицы signin_bin: uid | nick | status | remain | detail
    # Колонка статуса дополнена слева пробелами (формат %-12s), поэтому после значения идут пробелы и |. Допуск через [[:space:]]*.
    # Выбираем только строки FAIL/LOAD_ERR/AUTH_INVALID (ALREADY — идемпотентный успех, не считаем).
    printf '%s\n' "$OUT" | grep -E '\| (FAIL|LOAD_ERR|AUTH_INVALID)[[:space:]]*\|' || echo "  (нет fail)"
else
    exec "$BIN" "$AUTHS_DIR"
fi
