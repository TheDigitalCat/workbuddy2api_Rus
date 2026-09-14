#!/usr/bin/env python3
"""Одноразовый скрипт задания: chat_5 (5 диалогов с ИИ, +100 баллов +5 энергии).

Прогресс добивается отчётами о событии chat_request_send в {billing}/v2/report.
Форма скопирована из chatRequestEvent в report.go целиком (включая userId, без него тихо отбрасывается).
По умолчанию dry-run, реальная отправка только с --yes; по умолчанию добиваем до 5, есть --count N.

Использование
  python3 task_chat5.py <префикс uid>                # dry-run
  python3 task_chat5.py <префикс uid> --yes          # добрать до 5
  python3 task_chat5.py <префикс uid> --yes --count 2  # добрать только 2
"""
import sys, os, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "chat_5"
TARGET = 5


def main():
    ap = argparse.ArgumentParser(description="Задание chat_5: отправить N отчётов о диалогах до 5/5")
    ap.add_argument("account", help="префикс uid или ALL")
    ap.add_argument("--yes", action="store_true", help="подтвердить выполнение записывающих действий")
    ap.add_argument("--count", type=int, default=0,
                    help="число отчётов за раз (по умолчанию автодобой до 5)")
    ap.add_argument("--gap", type=float, default=1.05, help="пауза между отправками, сек")
    a = ap.parse_args()

    prefixes = []
    if a.account.upper() == "ALL":
        import glob
        prefixes = [os.path.basename(p)[10:18]
                    for p in sorted(glob.glob(tc.AUTHS + "/workbuddy-*.json"))]
    else:
        prefixes = [a.account]

    for pre in prefixes:
        c = tc.load_auth(pre)
        print(f"== {c['uid'][:8]} ({c['nick']}) ==")
        try:
            st = tc.task_status(c, TASK_CODE)
        except Exception as e:
            print(f"  [skip] сбой list_tasks: {e}")
            continue
        if st is None:
            print(f"  [skip] нет задания {TASK_CODE}")
            continue
        ast = st.get("accept_status")
        prog = (st.get("progress") or {})
        cur = prog.get("current", 0)
        target = prog.get("target", TARGET)
        if ast == "claimed" or cur >= target:
            print(f"  [skip] уже выполнено {cur}/{target} (accept_status={ast})")
            continue
        need = max(0, target - cur)
        if a.count > 0:
            need = min(need, a.count)
        if need <= 0:
            print("  [skip] отчёт не требуется")
            continue
        if not a.yes:
            print(f"  [dry-run] будет отправлено {need} chat_request_send "
                  f"(сейчас {cur}/{target}, добавьте --yes)")
            continue
        results = tc.report_activity(c, count=need, gap=a.gap)
        # Контрольное перечитывание
        st2 = tc.task_status(c, TASK_CODE)
        prog2 = (st2.get("progress") or {}) if st2 else {}
        print(f"  report x{need} -> {results}")
        print(f"  Контроль {TASK_CODE}: {prog2.get('current', 0)}/{prog2.get('target', target)} "
              f"accept_status={st2.get('accept_status') if st2 else '?'}")


if __name__ == "__main__":
    main()
