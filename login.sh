#!/usr/bin/env bash
# login.sh — WorkBuddy CN OAuth-вход → сохранение auth-файла
#
# Использование:
#   ./login.sh
#
# Процесс:
#   1. POST /v2/plugin/auth/state — получить URL авторизации (без PKCE, state выдаёт сервер)
#   2. Открыть URL в браузере и завершить вход
#   3. Вернуться сюда, нажать y → poll забирает token+uid+nickname → чек-ин → сохранение auths/workbuddy-<uid>.json
#   4. Перезапустить контейнер workbuddy2api, чтобы подхватить новый аккаунт
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="workbuddy2api"

mkdir -p "$AUTH_DIR"

# Утилита login: собираем только если отсутствует (после правок исходников вручную: go build -o login ./cmd/login)
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]]; then
    go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  Вход через WorkBuddy OAuth"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" url)

echo "Откройте следующую ссылку в браузере и завершите вход:"
echo ""
echo "  $AUTH_URL"
echo ""

if command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(скопировано в буфер обмена)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(скопировано в буфер обмена)"
fi

echo ""
read -rp "После завершения входа нажмите y для продолжения: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "Отменено"
    exit 1
fi

echo ""
echo "Получение token..."

RESULT=$("$LOGIN_BIN" poll) || {
    echo ""
    echo "Не удалось получить token. Возможные причины:"
    echo "  - вход ещё не завершён, а y уже нажат (повторите: ./login.sh)"
    echo "  - на странице входа ошибка (пришлите скриншот ошибки для разбора)"
    exit 1
}

TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['access_token'])")
REFRESH=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['refresh_token'])")
EXPIRES_IN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['expires_in'])")
DOMAIN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('domain',''))")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('uid',''))")
ENT_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('enterprise_id',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickname',''))")

if [[ -z "$USER_ID" ]]; then
    echo "Не удалось получить uid, проверьте валидность token"
    exit 1
fi

EXPIRES_AT=$(( $(date +%s) + EXPIRES_IN ))

# ─── Чек-ин (CN: POST codebuddy.cn/v2/billing/meter/daily-checkin, идемпотентный, не блокирует) ───
python3 - <<PYEOF
import json, urllib.request, urllib.error

req = urllib.request.Request(
    "https://www.codebuddy.cn/v2/billing/meter/daily-checkin",
    method="POST", data=b"{}",
    headers={
        "Authorization": "Bearer $TOKEN",
        "Accept": "application/json",
        "Content-Type": "application/json",
        "X-User-Id": "$USER_ID",
        **({"X-Enterprise-Id": "$ENT_ID", "X-Tenant-Id": "$ENT_ID"} if "$ENT_ID" else {}),
        **({"X-Domain": "$DOMAIN"} if "$DOMAIN" else {}),
    })
try:
    with urllib.request.urlopen(req, timeout=15) as r:
        body = json.loads(r.read().decode() or "{}")
    if body.get("code") == 0:
        data = body.get("data") or {}
        print(f"Чек-ин: успех {json.dumps(data, ensure_ascii=False)[:150]}")
    else:
        print(f"Чек-ин: {body.get('msg', json.dumps(body)[:150])}")
except urllib.error.HTTPError as e:
    # Бизнес-ошибки вида «уже отмечен» тоже идут через 4xx (на практике code=10001 "今天已签到")
    try:
        body = json.loads(e.read().decode() or "{}")
        print(f"Чек-ин: {body.get('msg', 'http %d' % e.code)}")
    except Exception:
        print(f"Чек-ин: http {e.code}")
except Exception as e:
    print(f"Чек-ин: {e}")
PYEOF

# ─── Сохранение auth-файла (формат чтения internal/auth) ─────────────────
AUTH_FILE="$AUTH_DIR/workbuddy-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    echo "Аккаунт уже существует (uid=${USER_ID}), учётные данные будут перезаписаны"
    ACTION="обновление"
else
    echo "Новый аккаунт (uid=${USER_ID}), создаём auth-файл"
    ACTION="создание"
fi
python3 - <<PYEOF
import json

auth = {
    "account": {
        "uid": "$USER_ID",
        "enterpriseId": "$ENT_ID",
        "nickname": "$NICKNAME"
    },
    "auth": {
        "accessToken": "$TOKEN",
        "refreshToken": "$REFRESH",
        "expiresAt": $EXPIRES_AT,
        "domain": "$DOMAIN"
    }
}
with open("$AUTH_FILE", "w") as f:
    json.dump(auth, f, indent=1)
print(f"Сохранено (${ACTION}): $AUTH_FILE")
PYEOF

# ─── Перезапуск сервиса ────────────────────────────────────────────
echo ""
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "Перезапуск $CONTAINER для подхвата нового аккаунта..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    # API_KEY читается из config.json (переменная в скрипте не задана, fallback — лишь заглушка, авторизацию не пройдёт)
    API_KEY=$(python3 -c "import json; print(json.load(open('config.json')).get('api_key',''))" 2>/dev/null)
    COUNT=$(curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer ${API_KEY:-test_key}" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
    echo "Сервис перезапущен, текущее число аккаунтов: $COUNT"
else
    echo "Контейнер $CONTAINER не запущен, auth-файл сохранён, будет загружен при следующем старте"
fi

echo ""
echo "============================================================"
echo "  Вход выполнен!"
echo "  UID: $USER_ID"
echo "  Nickname: ${NICKNAME:-(не получено)}"
echo "  Token: ${TOKEN:0:30}..."
echo "  Срок действия: $(date -d "@$EXPIRES_AT" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "$EXPIRES_AT")"
echo "============================================================"
