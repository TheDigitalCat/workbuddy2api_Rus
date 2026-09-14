#!/usr/bin/env python3
"""Одноразовый скрипт задания: first_buddy (забрать одного Buddy, +300 баллов +8 энергии).

Цепочка (скопирована из unlock-режима probe_active.py, проверено на практике):
  report(1 штука chat_request_send) -> buddy/agreement -> buddy/first
Критерий — поведенческие события сервера, а не статус accept.

Использование
  python3 task_first_buddy.py <префикс uid>            # dry-run
  python3 task_first_buddy.py <префикс uid> --yes      # выполнить по-настоящему
  python3 task_first_buddy.py ALL --yes            # весь пул (claimed пропускаем)
"""
import sys, os, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "first_buddy"


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)
    account = sys.argv[1]
    yes = "--yes" in sys.argv
    gap = 1.05

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
        if ast == "claimed":
            print(f"  [skip] {TASK_CODE} уже claimed, обработка не нужна")
            continue
        if not yes:
            print(f"  [dry-run] будет выполнено report + agreement + buddy/first (добавьте --yes; сейчас accept_status={ast})")
            continue

        # Цепочка записи
        st_code, code = tc.report_activity(c, count=1, gap=gap)[0]
        print(f"  report      -> {st_code} code={code}")
        time.sleep(2)
        st2, ag = tc.do_post(c, tc.chat_base(c), tc.PATH_BUDDY_AGREEMENT, {"agree": True})
        print(f"  agreement   -> {st2} code={ag.get('code')}")
        st3, bf = tc.do_post(c, tc.chat_base(c), tc.PATH_BUDDY_FIRST, {})
        d = bf.get("data") or {}
        print(f"  buddy/first -> {st3} {bf.get('msg')} credit={d.get('credit')} energy={d.get('energy')}")
        time.sleep(gap)


if __name__ == "__main__":
    main()
