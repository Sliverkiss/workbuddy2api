#!/usr/bin/env python3
"""一次性任务脚本：create_canvas（「设计创意」模式创建 1 个画布，+300 积分 +5 能量）.

桩点逆向结论（M1 里程碑报告，2026-09-13）：
  客户端在设计画布创建成功时经 {endpoint}/v2/report 上报以下事件（全部走
  wb.metrics.report -> EventService -> POST /v2/report，与 chat_request_send 同链路）：
    1. wbx_design_canvas_task_create   — ardot/create_design 工具完成时（ui-docs-viewer#318846,342384）
    2. wbx_design_conversation_create  — Home 创建设计会话后（home-BECKZUzd.js#246）
    3. wbx_design_chat_request_send    — 设计会话发送含画布上下文块的消息（ui-docs-viewer#318926）
    4. design_tab_click（映射为 web_element_click）— 切到「设计创意」Tab（ui-docs-viewer#318703）
  缺 userId 会被服务端 200 但静默丢弃（chat_5 先例），故必须带 userId。

实测（2026-09-13，<uid>）：
  - accept 后依次上报 4 种事件全部 200 code=0，回读 create_canvas 0/1 -> 1/1，
    accept_status=completed（✅ 进度点亮已复现）。
  - 但 claim（/v2/activity/growth/tasks/reward/claim）回 400 "task not completed"，
    与 Model_chat_GLM5.2 已知悬案同源 —— 列表 completed 与 claim 判定不是同一记录。
  - 未单独隔离「哪一条事件真正触发计数」：本号已 completed，隔离需另取一号。

用法
  python3 task_create_canvas.py <uid前缀>                # dry-run
  python3 task_create_canvas.py <uid前缀> --yes          # accept + 上报 4 种事件 + 回读
  python3 task_create_canvas.py <uid前缀> --yes --event wbx_design_canvas_task_create  # 只发某一种
"""
import sys, os, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "create_canvas"
TARGET = 1

DEFAULT_EVENTS = [
    "wbx_design_canvas_task_create",
    "wbx_design_conversation_create",
    "wbx_design_chat_request_send",
    "web_element_click",  # design_tab_click 被映射为该 eventCode
]


def canvas_event(event_code, auth, conversation_id=None):
    """按客户端源码形状组装单个 canvas 埋点事件。"""
    now = int(time.time() * 1000)
    cid = conversation_id or f"wb-canvas-{now}"
    rid = f"{cid}-{now}"
    if event_code == "wbx_design_canvas_task_create":
        return {
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "conversationId": cid, "requestId": rid,
            "source": "summon_keyword", "isCustomModel": False,
            "name": "", "inputLength": 12,
            "id": f"wbx-canvas-{now}", "cost": 0, "isSuccessful": True,
            "userId": auth["uid"],
        }
    if event_code == "wbx_design_conversation_create":
        return {
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "conversationId": cid, "id": "",
            "total": 1, "source": "design_tab",
            "imageMode": "ai",
            "designStyleId": "", "designStyleName": "",
            "userId": auth["uid"],
        }
    if event_code == "wbx_design_chat_request_send":
        return {
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "conversationId": cid, "requestId": rid,
            "mentionContexts": ["canvas"], "mentionContextCount": 1,
            "userId": auth["uid"],
        }
    if event_code == "web_element_click":  # design_tab_click 映射
        return {
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "pageURL": "wbx_design_home", "elementId": "wbx_design_tab",
            "elementName": "切换到设计创意 Tab",
            "userId": auth["uid"],
        }
    raise ValueError(f"unknown event: {event_code}")


def main():
    ap = argparse.ArgumentParser(description="create_canvas 任务：上报画布创建埋点完成 1/1")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--event", action="append", default=None,
                    help="要上报的事件码（可多次）；默认依次上 4 种")
    ap.add_argument("--gap", type=float, default=1.05)
    a = ap.parse_args()

    events = a.event or DEFAULT_EVENTS

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
            print(f"  [dry-run] 将 accept + 依次上报 {events} "
                  f"(当前 {cur}/{target}，加 --yes 生效)")
            for ev in events:
                print(f"    -> {json.dumps(canvas_event(ev, c), ensure_ascii=False)}")
            continue

        # 1. 先 accept（任务进度从接单开始计）
        if ast == "not_accepted":
            st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
            print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
            time.sleep(1.05)

        # 2. 依次上报事件
        for i, ev in enumerate(events):
            payload = [canvas_event(ev, c)]
            st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
            print(f"  report[{ev}] -> {st_r} {r_r if not isinstance(r_r, dict) else r_r.get('code')}")
            if i < len(events) - 1:
                time.sleep(a.gap)

        time.sleep(1.0)

        # 3. 回读确认
        st2 = tc.task_status(c, TASK_CODE)
        prog2 = (st2.get("progress") or {}) if st2 else {}
        cur2 = prog2.get("current", 0)
        ast2 = st2.get("accept_status") if st2 else "?"
        print(f"  回读 {TASK_CODE}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")
        if ast2 not in ("claimed",) and cur2 >= target:
            print("  → 任务已满足，下一步可 claim")


if __name__ == "__main__":
    import json
    main()