#!/usr/bin/env python3
"""Одноразовый скрипт задания: RichMeow_Chat (1 диалог в десктопной версии, +100 баллов +5 энергии + лимитированный слепой бокс Buddy).

Задание: сначала проверить, отличается ли eventCode от chat_request_send (искать RichMeow
в probe_active.py / коде Go — выделенного eventCode не найдено, в REPORT §3.2 тоже помечено
«только десктоп (не вскрыт)»).

Вывод замеров (REPORT §3.2, 2026-09-11):
  - после accept многократные отчёты chat_request_send на 3 аккаунтах не засчитываются (progress стоит 0/1).
  - подозрение: у десктопного варианта свой канал отчётности: queuePendingGrowthTelemetry вешает growthEvent
    в extra_vars чат-запроса и едет вверх вместе с запросом (REPORT §1.1), а не через /v2/report.
  - скрипт по умолчанию пробует отправить 1 chat_request_send (цена пренебрежима); если останется 0/1 —
    считаем неавтоматизируемым скриптом.

Использование
  python3 task_richmeow.py <префикс uid>             # dry-run
  python3 task_richmeow.py <префикс uid> --yes       # отправить 1 отчёт и перечитать
"""
import sys, os, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "RichMeow_Chat"
TARGET = 1


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)
    account = sys.argv[1]
    yes = "--yes" in sys.argv

    prefixes = []
    if account.upper() == "ALL":
        import glob
        prefixes = [os.path.basename(p)[10:18]
                    for p in sorted(glob.glob(tc.AUTHS + "/workbuddy-*.json"))]
    else:
        prefixes = [account]

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
        # not_accepted — сначала accept
        if ast == "not_accepted" and yes:
            st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
            print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
            time.sleep(1.05)
        elif ast == "not_accepted":
            print(f"  [dry-run] сначала accept (сейчас not_accepted), затем отправить 1 отчёт")
        if not yes:
            print(f"  [dry-run] будет отправлен 1 chat_request_send (сейчас {cur}/{target}, добавьте --yes)")
            continue
        results = tc.report_activity(c, count=1, gap=1.05)
        print(f"  report x1 -> {results}")
        time.sleep(1.0)
        st2 = tc.task_status(c, TASK_CODE)
        prog2 = (st2.get("progress") or {}) if st2 else {}
        print(f"  Контроль {TASK_CODE}: {prog2.get('current', 0)}/{prog2.get('target', target)} "
              f"accept_status={st2.get('accept_status') if st2 else '?'}")


if __name__ == "__main__":
    main()
