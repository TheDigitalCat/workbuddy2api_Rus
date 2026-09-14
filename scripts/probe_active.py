#!/usr/bin/env python3
"""Зонд активного отчёта WorkBuddy / инструмент разморозки.

Назначение
  probe    только чтение: состояние growth-заданий аккаунта + streak
  report   запись: шлёт один chat_request_send в /v2/report (подсвечивает активность/серию + разблокирует first_buddy)
  unlock   запись: report -> agreement -> buddy/first (бесплатный Buddy для аккаунтов без него)

Использование
  python3 probe_active.py probe  00e26541
  python3 probe_active.py report 0225284f
  python3 probe_active.py unlock 0225284f --yes
  python3 probe_active.py report ALL --yes --gap 1.05   # весь пул

Примечание
  - по умолчанию dry-run: записывающие действия требуют явного --yes
  - интервал одного аккаунта на один интерфейс >=1.05с (--gap)
  - body обязан содержать поле userId, иначе сервер вернёт 200, но тихо отбросит
"""
import json, os, sys, time, glob, argparse, urllib.request, urllib.error

AUTHS = "/root/workbuddy2api/auths"
CHAT_BASE = "https://copilot.tencent.com"   # growth / report
BILL_BASE = "https://www.codebuddy.cn"      # billing / report

def cred(prefix):
    hits = glob.glob(os.path.join(AUTHS, f"workbuddy-{prefix}*.json"))
    if not hits:
        raise SystemExit(f"нет auth для {prefix}")
    d = json.load(open(hits[0]))
    a, acc = d["auth"], d["account"]
    return {"token": a["accessToken"], "domain": a.get("domain") or "",
            "uid": acc["uid"], "nick": acc.get("nickname", ""),
            "file": os.path.basename(hits[0])}

def call(c, method, path, body=None, host=CHAT_BASE, timeout=30):
    url = path if path.startswith("http") else host + path
    hdr = {"Authorization": "Bearer " + c["token"], "X-User-Id": c["uid"],
           "X-Domain": c["domain"], "Accept": "application/json",
           "Content-Type": "application/json",
           "User-Agent": "CLI/2.63.2 CodeBuddy/2.63.2",
           "Origin": "https://www.codebuddy.cn", "Referer": "https://www.codebuddy.cn/"}
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, headers=hdr, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        t = e.read().decode("utf-8", "replace")
        try:    return e.code, json.loads(t)
        except Exception: return e.code, {"raw": t[:300]}
    except Exception as e:
        return -1, {"err": repr(e)}

def chat_event(c, conversation_id=None):
    """Форма события chat_request_send клиента (скопирована из CLI, userId не удалять)."""
    now = int(time.time() * 1000)
    cid = conversation_id or f"wb-active-{now}"
    return {"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
            "mode": "craft", "conversationId": cid, "requestId": cid,
            "inputLength": 12, "requestModelId": "deepseek-v4-flash",
            "requestModelName": "DeepSeek V4 Flash", "isPlan": False,
            "isAutoExecuteTerminal": False, "isAutoModify": False,
            "codebaseEnable": False, "maxToken": 0, "maxSteps": 0, "temperature": 0,
            "maxRetries": 0, "mentionContexts": [], "knowledgeId": [],
            "knowledgeName": [], "codebaseId": "", "mentionContextCount": 0,
            "command": "", "expertId": "", "recommendId": "", "skillId": "",
            "skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
            "traceId": "", "rootRequestId": cid, "parentConversationId": cid,
            "agentName": "default", "agentType": "conversation", "userId": c["uid"]}

def report_activity(c):
    """Подсвечивает активность/серию. Возвращает (status, code)."""
    st, r = call(c, "POST", "/v2/report", [chat_event(c)], host=BILL_BASE)
    return st, r.get("code")

def tasks(c):
    st, d = call(c, "GET", "/v2/activity/growth/tasks")
    out = {}
    for t in (d.get("data", {}).get("tasks") or []):
        out[t["task_code"]] = (t["accept_status"], t.get("progress"))
    return out

def unlock(c, n_reports=6):
    """Разморозка аккаунтов без Buddy: report -> agreement -> buddy/first -> chat_5"""
    st, code = report_activity(c)
    print(f"  report      -> {st} code={code}")
    time.sleep(2)
    st, ag = call(c, "POST", "/activity/growth/buddy/agreement", {"agree": True})
    print(f"  agreement   -> {st} code={ag.get('code')}")
    st, bf = call(c, "POST", "/activity/growth/buddy/first", {})
    d = bf.get("data") or {}
    print(f"  buddy/first -> {st} {bf.get('msg')} credit={d.get('credit')} energy={d.get('energy')}")
    return bf

def main():
    ap = argparse.ArgumentParser(description="Зонд активности WorkBuddy: probe/report/unlock")
    ap.add_argument("action", choices=["probe", "report", "unlock"], help="режим: probe — опрос, report — отчёт, unlock — разморозка")
    ap.add_argument("account", help="префикс uid или ALL")
    ap.add_argument("--yes", action="store_true", help="подтвердить выполнение записывающих действий")
    ap.add_argument("--gap", type=float, default=1.05, help="пауза между аккаунтами, сек")
    a = ap.parse_args()

    if a.account.upper() == "ALL":
        prefixes = [os.path.basename(p)[10:18]
                    for p in sorted(glob.glob(AUTHS + "/workbuddy-*.json"))]
    else:
        prefixes = [a.account]

    for pre in prefixes:
        c = cred(pre)
        print(f"== {c['uid'][:8]} ({c['nick']}) ==")
        if a.action == "probe":
            t = tasks(c)
            st, s = call(c, "GET", "/activity/growth/streak")
            print("  first_buddy:", t.get("first_buddy"),
                  "| RichMeow_Chat:", t.get("RichMeow_Chat"),
                  "| chat_5:", t.get("chat_5"))
            print("  streak.days:", (s.get("data", {}).get("streak", {}) or {}).get("days"))
        elif a.action == "report":
            if not a.yes:
                print("  [dry-run] будет отправлен 1 chat_request_send (добавьте --yes для выполнения)")
            else:
                print("  report ->", report_activity(c))
        else:  # unlock
            if not a.yes:
                print("  [dry-run] будет выполнено report + agreement + buddy/first (добавьте --yes для выполнения)")
            else:
                unlock(c)
        time.sleep(a.gap)

if __name__ == "__main__":
    main()
