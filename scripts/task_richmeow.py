#!/usr/bin/env python3
"""一次性任务脚本：RichMeow_Chat（「桌面端对话 1 次」，+100 积分 +5 能量 +限定 Buddy 盲盒，reward_buddy）.

任务语义（task_desc 原文）：**桌面端对话 1 次**（reward_buddy=true，限定 Buddy 盲盒）。

桩点逆向结论（M14 里程碑，2026-09-13，终批收尾）：
  - 全客户端静态检索 **零 richmeow/meow 专属事件码**：`meow` 仅出现在 emoji 词典
    （😺 grinning_cat 关键词），main/renderer/cli/native 均无 RichMeow 业务域字面量。
  - 106 项 EVENT_NAME_MAPPING 中与「对话」相关的只有 `chat_request_send`（历史实测
    3 账号多次上报均不点亮本任务，见 task_richmeow.py 说明）。
  - 桌面端唯一可判定的标识 = UA `WorkBuddy/<appVersion>`（commonFields.userAgent /
    os 来自 renderer navigator，随 /v2/report 上行）。因此「桌面端对话 1 次」的
    telemetry 可表达上限 = chat_request_send + userAgent=WorkBuddy/… + os=Linux x86_64。
  - 本任务 reward_buddy=true + template_id_fixed=11，与 black_cat=10 / Hp_Appearance=23 /
    Expert_Philanthropy=22 同族（M0/M8/M10 已定案：判据在服务端业务副作用，
    盲盒发放需真实桌面端业务动作 + 专用业务 API，telemetry 不可造）。

实测（<uid>，2026-09-13）：
  候选1 = chat_request_send（mode=desktop，payload 带 userAgent/os）→ 200 code=0，
    回读 0/1 不动。
  候选2 = chat_request_send 修正：mode 取字典合法值 craft + HTTP 头带桌面标识
    （User-Agent: WorkBuddy/2.63.2, X-Product-Code: workbuddy；等价桌面端
    EventExtraHeadersProvider 注入）→ 验证服务端是否以「桌面端」标识计数。

用法
  python3 task_richmeow.py <uid前缀>            # dry-run（打载荷，不发）
  python3 task_richmeow.py <uid前缀> --yes           # 候选2（mode=craft + 桌面头）
  python3 task_richmeow.py <uid前缀> --yes --cand 1  # 候选1（mode=desktop, payload 头）
"""
import sys, os, json, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "RichMeow_Chat"
TARGET = 1

DESKTOP_UA = "WorkBuddy/" + tc.CLIENT_UA.split(" ")[-1].split("/")[-1]  # WorkBuddy/2.63.2
DESKTOP_OS = "Linux x86_64"


def desktop_headers(auth, mode="craft"):
    """桌面端上报头：User-Agent=WorkBuddy/x.y.z + X-Product-Code + 认证（模拟 CLI EventExtraHeadersProvider）。"""
    h = dict(tc._headers(auth))
    h["User-Agent"] = DESKTOP_UA
    h["X-Product-Code"] = "workbuddy"
    return h


def desktop_chat_event(auth, mode="craft"):
    """chat_request_send：mode 取字典合法值（craft/ask/expert），带桌面标识字段。"""
    now = int(time.time() * 1000)
    cid = f"wb-meow-{now}"
    return [{
        "eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
        "mode": mode,
        "conversationId": cid, "requestId": cid,
        "inputLength": 12, "requestModelId": "deepseek-v4-flash",
        "requestModelName": "DeepSeek V4 Flash", "isPlan": False,
        "isAutoExecuteTerminal": False, "isAutoModify": False,
        "codebaseEnable": False, "maxToken": 0, "maxSteps": 0, "temperature": 0,
        "maxRetries": 0, "mentionContexts": [], "knowledgeId": [],
        "knowledgeName": [], "codebaseId": "", "mentionContextCount": 0,
        "command": "", "expertId": "", "recommendId": "", "skillId": "",
        "skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
        "traceId": "", "rootRequestId": cid, "parentConversationId": cid,
        "agentName": "default", "agentType": "conversation",
        "userAgent": DESKTOP_UA, "os": DESKTOP_OS,
        "userId": auth["uid"],
    }]


def main():
    ap = argparse.ArgumentParser(description="RichMeow_Chat 任务：桌面端对话 1 次")
    ap.add_argument("account", help="uid 前缀")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--cand", type=int, default=2, choices=[1, 2],
                    help="候选：1=mode=desktop payload 标识，2=craft + 桌面 HTTP 头（默认）")
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

    cand = a.cand
    if not a.yes:
        print(f"  [dry-run] accept(若需) + 候选{cand} chat_request_send"
              f"（当前 {cur}/{target}，加 --yes 生效）")
        if cand == 1:
            print(f"    mode=desktop, userAgent={DESKTOP_UA}, os={DESKTOP_OS}")
        else:
            print(f"    mode=craft + HTTP 头 User-Agent={DESKTOP_UA}, X-Product-Code=workbuddy")
        sys.exit(0)

    if ast == "not_accepted":
        st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
        print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
        time.sleep(1.05)

    payload = desktop_chat_event(c, mode="craft" if cand == 2 else "desktop")
    if cand == 2:
        st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload,
                                headers=desktop_headers(c))
    else:
        st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
    sc = r_r.get("code") if isinstance(r_r, dict) else r_r
    print(f"  report[chat_request_send #{'desktop+headers' if cand == 2 else 'desktop'}] -> {st_r} code={sc}")

    time.sleep(2.0)
    st2 = tc.task_status(c, TASK_CODE)
    prog2 = (st2.get("progress") or {}) if st2 else {}
    cur2 = prog2.get("current", 0)
    ast2 = st2.get("accept_status") if st2 else "?"
    print(f"  回读 {TASK_CODE}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")


if __name__ == "__main__":
    main()