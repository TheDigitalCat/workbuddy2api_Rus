#!/usr/bin/env python3
"""Одноразовый скрипт задания: Model_chat_GLM5.2 (1 диалог с моделью «GLM-5.2», +100 баллов +5 энергии).

Шаги (подтверждено замерами):
  1. accept: POST /v2/activity/growth/tasks/accept {"task_codes":["Model_chat_GLM5.2"]}
     (без accept поведенческое событие тоже подсветило бы прогресс, но с accept состояние корректнее)
  2. реальный диалог: POST {chat}/v2/chat/completions {model:"glm-5.2", stream:true}
     —— модель glm-5.2 в списке реально есть, SSE отвечает 200 нормально
  3. отправить один chat_request_send (поля модели выровнены под glm-5.2) для триггера progress
     —— если критерий задания опирается на отчёт о событии, этого хватит; реальный диалог — прямое
     доказательство «успешного диалога с GLM-5.2»
  4. перечитать progress; при current>=target подсказать claim

Использование
  python3 task_model_chat.py <префикс uid>            # dry-run
  python3 task_model_chat.py <префикс uid> --yes      # accept + реальный диалог + отчёт
"""
import sys, os, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "Model_chat_GLM5.2"
MODEL_ID = "glm-5.2"
MODEL_NAME = "GLM-5.2"
TARGET = 1


def main():
    ap = argparse.ArgumentParser(description="Задание Model_chat_GLM5.2")
    ap.add_argument("account", help="префикс uid или ALL")
    ap.add_argument("--yes", action="store_true", help="подтвердить выполнение записывающих действий")
    ap.add_argument("--prompt", default="hi，请回复一句话", help="промпт для GLM-5.2")
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
        if not a.yes:
            print(f"  [dry-run] будет выполнено accept + один реальный диалог GLM-5.2 + отчёт chat_request_send "
                  f"(сейчас {cur}/{target}, добавьте --yes)")
            continue

        # 1. accept
        st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
        print(f"  accept        -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
        time.sleep(1.05)

        # 2. реальный диалог (GLM-5.2)
        st_chat, first = tc.chat_completion(c, model_id=MODEL_ID, prompt=a.prompt)
        print(f"  chat glm-5.2  -> {st_chat} {first[:60]!r}")

        # 3. отправить один chat_request_send (поля модели выровнены под glm-5.2)
        results = tc.report_activity(c, count=1, gap=1.05,
                                     model_id=MODEL_ID, model_name=MODEL_NAME)
        print(f"  report x1     -> {results}")
        time.sleep(1.0)

        # 4. перечитать
        st2 = tc.task_status(c, TASK_CODE)
        prog2 = (st2.get("progress") or {}) if st2 else {}
        cur2 = prog2.get("current", 0)
        ast2 = st2.get("accept_status") if st2 else "?"
        print(f"  Контроль {TASK_CODE}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")
        if ast2 not in ("claimed",) and cur2 >= target:
            print("  → задание выполнено, награду можно забрать вручную или позже вызовом claim_reward")


if __name__ == "__main__":
    main()
