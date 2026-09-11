#!/usr/bin/env python3
"""一次性任务脚本：Model_chat_GLM5.2（体验「GLM-5.2」模型对话 1 次，+100 积分 +5 能量）.

步骤（实测确认）：
  1. accept：POST /v2/activity/growth/tasks/accept {"task_codes":["Model_chat_GLM5.2"]}
     （不 accept 也会被行为事件点亮，但进度状态更规范，先 accept）
  2. 真实对话：POST {chat}/v2/chat/completions {model:"glm-5.2", stream:true}
     —— 模型列表实测存在 glm-5.2，SSE 200 正常回包
  3. 上报一条 chat_request_send（requestModelId=glm-5.2）触发 progress
     —— 若任务判据靠事件上报，这条即可点亮；真实对话是「使用 GLM-5.2 成功对话」的最直接证据
  4. 回读 progress；current>=target 后提示 claim

用法
  python3 task_model_chat.py <uid前缀>            # dry-run
  python3 task_model_chat.py <uid前缀> --yes      # accept + 真实对话 + 上报
"""
import sys, os, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "Model_chat_GLM5.2"
MODEL_ID = "glm-5.2"
MODEL_NAME = "GLM-5.2"
TARGET = 1


def main():
    ap = argparse.ArgumentParser(description="Model_chat_GLM5.2 任务")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--prompt", default="hi，请回复一句话", help="发给 GLM-5.2 的提示词")
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
            print(f"  [skip] list_tasks 失败: {e}")
            continue
        if st is None:
            print(f"  [skip] 无 {TASK_CODE} 任务")
            continue
        ast = st.get("accept_status")
        prog = (st.get("progress") or {})
        cur = prog.get("current", 0)
        target = prog.get("target", TARGET)
        if ast == "claimed" or cur >= target:
            print(f"  [skip] 已完成 {cur}/{target} (accept_status={ast})")
            continue
        if not a.yes:
            print(f"  [dry-run] 将 accept + GLM-5.2 真实对话一次 + 上报 chat_request_send "
                  f"(当前 {cur}/{target}，加 --yes 生效)")
            continue

        # 1. accept
        st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
        print(f"  accept        -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
        time.sleep(1.05)

        # 2. 真实对话（GLM-5.2）
        st_chat, first = tc.chat_completion(c, model_id=MODEL_ID, prompt=a.prompt)
        print(f"  chat glm-5.2  -> {st_chat} {first[:60]!r}")

        # 3. 上报一条 chat_request_send（模型字段对齐 glm-5.2）
        results = tc.report_activity(c, count=1, gap=1.05,
                                     model_id=MODEL_ID, model_name=MODEL_NAME)
        print(f"  report x1     -> {results}")
        time.sleep(1.0)

        # 4. 回读
        st2 = tc.task_status(c, TASK_CODE)
        prog2 = (st2.get("progress") or {}) if st2 else {}
        cur2 = prog2.get("current", 0)
        ast2 = st2.get("accept_status") if st2 else "?"
        print(f"  回读 {TASK_CODE}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")
        if ast2 not in ("claimed",) and cur2 >= target:
            print("  → 任务已满足，可手动或后续调用 claim_reward 领奖")


if __name__ == "__main__":
    main()