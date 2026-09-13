#!/usr/bin/env python3
"""一次性任务脚本：black_cat（「参与『夜猫子』夜间折扣活动」，+0 积分，reward_buddy）.

任务语义（task_desc 原文）：**夜间23点-次日8点，新建对话并成功使用「GLM-5.2」，每天 1 次，累计 3 天。**
到 3/3 需 3 个不同自然日各在 23:00-08:00（CST）用 GLM-5.2 成功对话。

桩点逆向（M13 里程碑，2026-09-13）：
  - `chat_request_send` 是对话发送 telemetry（M0.5/M5 已实证可点亮 chat_5/Model_chat 类
    普通任务），但 black_cat 是 reward_buddy=true + template_id_fixed=10 特殊族（同
    Expert_Philanthropy=22 / Hp_Appearance=23 / RichMeow_Chat=11）。
  - 关键差异：**时段判定在服务端** —— 客户端只上报事件，服务端按自己的时钟判夜猫
    时段；`requestModelId` 是否带 `glm-5.2` 走「真实 chat/completions」是唯一带
    模型成功用法的权威信号（M0：纯真实对话不计数…仅对 model_chat 普通族成立；对
    reward_buddy 族无独立判据）。
  - 真实对话通道：POST {chatBase}/v2/chat/completions {model:"glm-5.2", stream:true}
    （task_common.chat_completion）。

实测场景（<uid>，CST 05:5x 处于窗口内）：
  候选1 = 真实 GLM-5.2 chat/completions（服务端按当前时段判夜猫）+ 回读。
  候选2 = chat_request_send 带 requestModelId="glm-5.2"（telemetry 通道，对照组）。
  若 0/1 不动 → 判定 = reward_buddy=true 族同一阻塞（判据在服务端业务，需真实
  GUI + 服务端状态录「成功使用 GLM-5.2 且时段命中」）。

用法
  python3 task_black_cat.py <uid前缀>          # dry-run
  python3 task_black_cat.py <uid前缀> --yes     # accept + 真实 GLM-5.2 对话 + 回读
  python3 task_black_cat.py <uid前缀> --yes --code chat_request_send  # telemetry 对照
"""
import sys, os, json, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "black_cat"
TARGET = 3
MODEL_GLM = "glm-5.2"
MODEL_GLM_NAME = "GLM-5.2"


# 夜猫时段（CST）：23:00 - 次日 08:00
def within_night_window():
    try:
        import datetime as _dt
        now = _dt.datetime.now(_dt.timezone(_dt.timedelta(hours=8)))  # CST
        return now.hour >= 23 or now.hour < 8
    except Exception:
        return None


def main():
    ap = argparse.ArgumentParser(description="black_cat 任务：夜间(23-8点)用 GLM-5.2 对话，累计 3 天")
    ap.add_argument("account", help="uid 前缀")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--code", default="real_chat", choices=["real_chat", "chat_request_send"])
    a = ap.parse_args()

    c = tc.load_auth(a.account)
    print(f"== {c['uid'][:8]} ({c['nick']}) ==")

    try:
        st = tc.task_status(c, TASK_CODE)
    except Exception as e:
        print(f"  [skip] list_tasks 失败: {e}")
        sys.exit(1)
    if st is None:
        print(f"  [skip] 无 {TASK_CODE} 任务")
        sys.exit(0)
    ast = st.get("accept_status")
    prog = (st.get("progress") or {})
    cur = prog.get("current", 0)
    target = prog.get("target", TARGET)
    if ast == "claimed" or cur >= target:
        print(f"  [skip] 已完成 {cur}/{target} (accept_status={ast})")
        sys.exit(0)

    in_window = within_night_window()
    print(f"  时段判定(CST 23:00-08:00): {'✅ 处于夜猫窗口' if in_window else '❌ 不在窗口（本次只能记为时段不符）'}"
          f"  [实测时间 {time.strftime('%Y-%m-%d %H:%M:%S')} CST]")

    if not a.yes:
        print(f"  [dry-run] accept(若需) + {a.code}（当前 {cur}/{target}，加 --yes 生效）")
        if a.code == "real_chat":
            print(f"    -> POST {tc.PATH_CHAT} {{model:{MODEL_GLM}, stream:true}}")
        else:
            print(f"    -> POST /v2/report chat_request_send (requestModelId={MODEL_GLM})")
        sys.exit(0)

    if ast == "not_accepted":
        st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
        print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
        time.sleep(1.05)

    if a.code == "real_chat":
        if not in_window:
            print(f"  [warn] 当前不在夜猫窗口，真实对话仍尝试（服务端判时）")
        st_chat, first = tc.chat_completion(c, model_id=MODEL_GLM, prompt="夜猫子测试，回复一个字即可", max_tokens=32)
        print(f"  chat[glm-5.2] -> {st_chat} {first[:60]!r}")
    else:
        payload = tc.chat_event(c, model_id=MODEL_GLM, model_name=MODEL_GLM_NAME, mode="night")
        payload = payload if isinstance(payload, list) else [payload]
        st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
        print(f"  report[chat_request_send #{MODEL_GLM}] -> {st_r} "
              f"code={r_r.get('code') if isinstance(r_r, dict) else r_r}")

    time.sleep(2.0)
    st2 = tc.task_status(c, TASK_CODE)
    prog2 = (st2.get("progress") or {}) if st2 else {}
    cur2 = prog2.get("current", 0)
    ast2 = st2.get("accept_status") if st2 else "?"
    print(f"  回读 {TASK_CODE}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")


if __name__ == "__main__":
    main()