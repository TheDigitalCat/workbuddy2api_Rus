#!/usr/bin/env python3
"""Общая библиотека одноразовых скриптов балловых заданий.

В том же стиле, что probe_active.py: читает учётные данные аккаунтов из auths/,
оборачивает запросы доменов growth / report для переиспользования в task_*.py.
По умолчанию везде dry-run (записывающие действия разрешаются явным --yes
в вызывающем скрипте).

Авторитетный источник эндпоинтов (код Go + текущие замеры):
  - домен chat (copilot.tencent.com): growth / tasks / buddy / streak / chat/completions
  - домен billing (www.codebuddy.cn): /v2/report
  - accept : POST /v2/activity/growth/tasks/accept  {"task_codes":[code]}
  - claim  : POST /v2/activity/growth/tasks/reward/claim {"task_code":code}
"""
import json, os, time, glob, urllib.request, urllib.error

AUTHS = "/root/workbuddy2api/auths"
CHAT_BASE = "https://copilot.tencent.com"   # growth / tasks / buddy / streak / chat
BILL_BASE = "https://www.codebuddy.cn"      # report / billing

# Константы домена growth (сверены с travel.go / report.go и текущими замерами)
PATH_LIST_TASKS     = "/v2/activity/growth/tasks"
PATH_ACCEPT_TASKS   = "/v2/activity/growth/tasks/accept"
PATH_CLAIM_REWARD   = "/v2/activity/growth/tasks/reward/claim"
PATH_BUDDY_FIRST    = "/activity/growth/buddy/first"
PATH_BUDDY_AGREEMENT = "/activity/growth/buddy/agreement"
PATH_STREAK         = "/activity/growth/streak"
PATH_REPORT         = "/v2/report"
PATH_CHAT           = "/v2/chat/completions"

CLIENT_UA = "CLI/2.63.2 CodeBuddy/2.63.2"


def load_auth(uid_or_file: str) -> dict:
    """Загрузить учётные данные аккаунта из auths/: uid_or_file — префикс uid или имя файла в auths.

    Возвращает пятёрку {token, uid, domain, nick, file}.
    """
    if os.path.sep in uid_or_file or uid_or_file.endswith(".json"):
        p = uid_or_file
        if not os.path.isabs(p):
            p = os.path.join(AUTHS, p)
    else:
        pre = uid_or_file
        hits = glob.glob(os.path.join(AUTHS, f"workbuddy-{pre}*.json"))
        if not hits:
            raise SystemExit(f"нет auth для {pre}")
        p = hits[0]
    d = json.load(open(p))
    a, acc = d["auth"], d["account"]
    return {"token": a["accessToken"], "domain": a.get("domain") or "",
            "uid": acc["uid"], "nick": acc.get("nickname", ""),
            "file": os.path.basename(p)}


def chat_base(auth: dict) -> str:
    return auth.get("chat_base") or CHAT_BASE


def billing_base(auth: dict) -> str:
    return auth.get("billing_base") or BILL_BASE


def _headers(auth: dict) -> dict:
    hdr = {"Authorization": "Bearer " + auth["token"],
           "Accept": "application/json",
           "Content-Type": "application/json",
           "User-Agent": CLIENT_UA,
           "Origin": "https://www.codebuddy.cn",
           "Referer": "https://www.codebuddy.cn/"}
    if auth.get("uid"):
        hdr["X-User-Id"] = auth["uid"]
    if auth.get("domain"):
        hdr["X-Domain"] = auth["domain"]
    return hdr


def _request(auth, method, base, path, body=None, headers=None, timeout=30):
    url = path if path.startswith("http") else base + path
    hdr = _headers(auth)
    if headers:
        hdr.update(headers)
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, headers=hdr, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        t = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(t)
        except Exception:
            return e.code, {"raw": t[:300]}
    except Exception as e:
        return -1, {"err": repr(e)}


def do_get(auth, base, path, headers=None) -> tuple:
    """GET-запрос на чтение, возвращает (status, dict)."""
    return _request(auth, "GET", base, path, None, headers)


def do_post(auth, base, path, body, headers=None) -> tuple:
    """POST-запрос на запись, возвращает (status, dict). body — dict."""
    return _request(auth, "POST", base, path, body, headers)


def list_tasks(auth) -> list:
    """Полный список заданий GET /v2/activity/growth/tasks (элементы — исходные dict)."""
    st, d = do_get(auth, chat_base(auth), PATH_LIST_TASKS)
    if st != 200:
        raise RuntimeError(f"list_tasks http={st}")
    tasks = (d.get("data", {}) or {}).get("tasks") or []
    return tasks


def task_status(auth, task_code) -> dict | None:
    """Текущее состояние одного задания; если не найдено — None."""
    for t in list_tasks(auth):
        if t.get("task_code") == task_code:
            return t
    return None


def accept_tasks(auth, task_codes) -> tuple:
    """POST accept заданий (not_accepted → accepted). Возвращает (status, resp)."""
    return do_post(auth, chat_base(auth), PATH_ACCEPT_TASKS,
                   {"task_codes": task_codes})


def claim_reward(auth, task_code) -> tuple:
    """POST claim для получения награды (после complete задания). Повторное получение вернёт бизнес-ошибку, безопасно."""
    return do_post(auth, chat_base(auth), PATH_CLAIM_REWARD,
                   {"task_code": task_code})


def get_streak(auth) -> int:
    """GET /activity/growth/streak — дни серии подряд (только чтение, oracle). При сбое возвращает -1."""
    st, d = do_get(auth, chat_base(auth), PATH_STREAK)
    if st != 200:
        return -1
    return (d.get("data", {}).get("streak", {}) or {}).get("days", 0)


def chat_event(auth, conversation_id=None, model_id="deepseek-v4-flash",
               model_name="DeepSeek V4 Flash", mode="craft"):
    """Полная форма события chat_request_send клиента (скопирована из report.go / probe_active.py).

    Обязательно содержит userId (= uid аккаунта), без него сервер вернёт 200, но тихо отбросит.
    model_id/name можно менять (напр. GLM-5.2) для выравнивания под model_chat.
    """
    now = int(time.time() * 1000)
    cid = conversation_id or f"task-{now}"
    return {"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
            "mode": mode, "conversationId": cid, "requestId": cid,
            "inputLength": 12, "requestModelId": model_id,
            "requestModelName": model_name, "isPlan": False,
            "isAutoExecuteTerminal": False, "isAutoModify": False,
            "codebaseEnable": False, "maxToken": 0, "maxSteps": 0, "temperature": 0,
            "maxRetries": 0, "mentionContexts": [], "knowledgeId": [],
            "knowledgeName": [], "codebaseId": "", "mentionContextCount": 0,
            "command": "", "expertId": "", "recommendId": "", "skillId": "",
            "skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
            "traceId": "", "rootRequestId": cid, "parentConversationId": cid,
            "agentName": "default", "agentType": "conversation", "userId": auth["uid"]}


def report_activity(auth, count=1, gap=1.05, model_id="deepseek-v4-flash",
                    model_name="DeepSeek V4 Flash", mode="craft") -> list:
    """Отправить count штук chat_request_send в {billing}/v2/report.

    Интервал каждый раз >= gap секунд (по умолчанию 1.05, тот же порог троттлинга,
    что в probe_active.py для того же интерфейса).
    Возвращает сводку [(status, code), ...].
    """
    out = []
    for i in range(count):
        ev = chat_event(auth, model_id=model_id, model_name=model_name, mode=mode)
        st, r = do_post(auth, billing_base(auth), PATH_REPORT, [ev])
        out.append((st, r.get("code") if isinstance(r, dict) else None))
        if i < count - 1:
            time.sleep(gap)
    return out


def chat_completion(auth, model_id="glm-5.2", prompt="hi", max_tokens=32,
                    timeout=60) -> tuple:
    """Один реальный диалог POST {chat}/v2/chat/completions (stream:true).

    Сервер требует стриминг (тот же порог, что в payload.go), здесь построчно читаем SSE до done.
    Возвращает (status, first_content). Нужно для «одного реального диалога» Model_chat_GLM5.2.
    """
    body = {"model": model_id, "messages": [{"role": "user", "content": prompt}],
            "stream": True, "max_tokens": max_tokens}
    hdr = {"Accept": "text/event-stream"}  # SSE
    url = chat_base(auth) + PATH_CHAT
    req_headers = _headers(auth)
    req_headers.update(hdr)
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers=req_headers, method="POST")
    first = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            status = r.status
            for raw in r:
                line = raw.decode("utf-8", "replace")
                if line.startswith("data: "):
                    payload = line[6:].strip()
                    if payload in ("[DONE]", ""):
                        continue
                    try:
                        obj = json.loads(payload)
                        delta = (obj.get("choices") or [{}])[0].get("delta") or {}
                        content = delta.get("content") or ""
                        if content and not first:
                            first = content
                    except Exception:
                        pass
            return status, first
    except urllib.error.HTTPError as e:
        t = e.read().decode("utf-8", "replace")
        return e.code, t[:200]
    except Exception as e:
        return -1, repr(e)[:200]
