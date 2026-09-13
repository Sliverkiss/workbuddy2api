#!/usr/bin/env python3
"""一次性任务脚本：Buddy_App（「发现应用」，+100）+ Buddy_App_QQ（「企鹅教师助手」，+50）.

桩点逆向结论（M11 里程碑，2026-09-13）：
  均为普通任务（reward_buddy=false, template_id_fixed=0），判定链路 = 客户端 telemetry
  经 POST {billing}/v2/report，必带 userId。事件码在 ui-docs-viewer 的
  AgentTelemetryEvents 枚举 + EVENT_NAME_MAPPING 确认（原样透传）：
    buddyapp_discover_click   —— 打开 Buddy 应用切换器（payload {}）
    buddyapp_show             —— 应用列表项曝光 {elementId, elementName, position}
    buddyapp_enter_click      —— 点击进入某 Buddy 应用 {elementId=applicationId,
                                 elementName=templateName, position, isFirstPage}
    buddyapp_menu_click       —— 左侧菜单点击 {elementId, elementName}
  Buddy_App_QQ 的目标应用已实测确认：
    open-platform search API（POST {sso}/api/v1/open-platform/buddy_application/search，
    读权限对当前 token 开放）命中 application_id=cb_y5Dy46tPQGGWtueMxXbe
    = 「企鹅教师助手」。
  elementId 用 application_id（resolveBuddyAppIdentity：applicationId 优先，
  回落 templateId；本任务固定 templateId cb_y5Dy46tPQGGWtueMxXbe 与 application_id
  相等）。isFirstPage=`toBuddyAppFirstEnter(authorized)` → "0"/"1"。

实测（2026-09-13，<uid>）：
  见脚本末尾 stdout + (M11-Buddy_App).md。

用法
  python3 task_buddy_app.py <uid前缀>            # dry-run（打载荷，不发）
  python3 task_buddy_app.py <uid前缀> --yes --task Buddy_App --code buddyapp_discover_click
  python3 task_buddy_app.py <uid前缀> --yes --task Buddy_App_QQ   # 默认 enter_click
"""
import sys, os, json, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

# Buddy_App（通用发现应用）默认候选事件，按顺序试到点亮为止
BUDDY_APP_EVENTS = ["buddyapp_discover_click", "buddyapp_show"]
# Buddy_App_QQ（企鹅教师助手）主事件
QQ_ENTER_TEMPLATE_ID = "cb_y5Dy46tPQGGWtueMxXbe"
QQ_APP_NAME = "企鹅教师助手"
QQ_DEFAULT_EVENT = "buddyapp_enter_click"

OPEN_PLATFORM_HOST = "https://tencent.sso.codebuddy.cn"
PATH_BUDDY_LIST = "/api/v1/open-platform/buddy_application"
PATH_BUDDY_SEARCH = "/api/v1/open-platform/buddy_application/search"


def fetch_qq_application(auth):
    """只读查 open-platform 应用，确认 cb_y5Dy46tPQGGWtueMxXbe 的应用名等。"""
    try:
        st, r = tc.do_post(auth, OPEN_PLATFORM_HOST, PATH_BUDDY_SEARCH, {"keyword": "企鹅"})
        apps = ((r.get("data") or {}).get("applications") or []) if isinstance(r, dict) else []
        for ap in apps:
            if ap.get("application_id") == QQ_ENTER_TEMPLATE_ID:
                return ap.get("application_name") or QQ_APP_NAME
    except Exception as ex:
        print(f"  [warn] open-platform 查询失败({ex})，回落后备名")
    return QQ_APP_NAME


def buddy_event(event_code, auth, element_id="", element_name="", position=1,
                is_first_page="0", source="list"):
    """按客户端源码形状组装单个 buddyapp 事件（/v2/report 信封外字段）。"""
    now = int(time.time() * 1000)
    if event_code == "buddyapp_discover_click":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "userId": auth["uid"],
        }]
    if event_code == "buddyapp_show":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "elementId": element_id, "elementName": element_name,
            "position": position, "userId": auth["uid"],
        }]
    if event_code == "buddyapp_enter_click":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "elementId": element_id, "elementName": element_name,
            "position": position, "isFirstPage": is_first_page,
            "userId": auth["uid"],
        }]
    if event_code == "buddyapp_menu_click":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "elementId": element_id, "elementName": element_name,
            "userId": auth["uid"],
        }]
    if event_code == "buddyapp_exit_click":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "elementId": element_id, "elementName": element_name,
            "source": source, "userId": auth["uid"],
        }]
    raise ValueError(f"unknown event: {event_code}")


def run_one(auth, task_code, event_code, element_id, element_name, position, target):
    """单任务：accept(若需) → 上报 1 事件 → 回读。返回 (cur, ast)。"""
    st = tc.task_status(auth, task_code)
    if st is None:
        print(f"  [skip] 无 {task_code} 任务")
        return None, None
    prog = (st.get("progress") or {})
    cur = prog.get("current", 0)
    ast = st.get("accept_status")
    if ast == "claimed" or cur >= target:
        print(f"  [skip] 已完成 {cur}/{target} (accept_status={ast})")
        return cur, ast
    if ast == "not_accepted":
        st_a, r_a = tc.accept_tasks(auth, [task_code])
        print(f"  accept {task_code} -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
        time.sleep(1.05)
    payload = buddy_event(event_code, auth, element_id, element_name, position)
    st_r, r_r = tc.do_post(auth, tc.billing_base(auth), tc.PATH_REPORT, payload)
    sc = r_r.get("code") if isinstance(r_r, dict) else r_r
    print(f"  report[{task_code} {event_code}] ({element_id}) -> {st_r} code={sc}")
    time.sleep(2.0)
    st2 = tc.task_status(auth, task_code)
    prog2 = (st2.get("progress") or {}) if st2 else {}
    cur2 = prog2.get("current", 0)
    ast2 = st2.get("accept_status") if st2 else "?"
    print(f"  回读 {task_code}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")
    return cur2, ast2


def main():
    ap = argparse.ArgumentParser(description="Buddy_App / Buddy_App_QQ 任务脚本")
    ap.add_argument("account", help="uid 前缀")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--task", default="Buddy_App", choices=["Buddy_App", "Buddy_App_QQ"],
                    help="目标任务")
    ap.add_argument("--code", default=None,
                    choices=["buddyapp_discover_click", "buddyapp_show",
                             "buddyapp_enter_click", "buddyapp_menu_click",
                             "buddyapp_exit_click"])
    a = ap.parse_args()

    c = tc.load_auth(a.account)
    print(f"== {c['uid'][:8]} ({c['nick']}) ==")

    if a.task == "Buddy_App":
        target = 1
        events = [a.code] if a.code else BUDDY_APP_EVENTS
        if not a.yes:
            st = tc.task_status(c, a.task)
            cur = ((st.get("progress") or {}).get("current", 0)) if st else 0
            print(f"  [dry-run] 将依序尝试事件 {events}（默认 discover→show），"
                  f"elementId=任一真实 applicationId (当前 {cur}/{target}，加 --yes 生效)")
            sys.exit(0)
        element_id, element_name = "cb_sPrtTxKjCrwryRkAMdaJ", "通达信"  # open-platform 列表首项
        for ev in events:
            print(f"  -- 尝试 {ev} --")
            run_one(c, a.task, ev, element_id, element_name, 1, target)
            st_after = tc.task_status(c, a.task)
            prog = (st_after.get("progress") or {}) if st_after else {}
            if prog.get("current", 0) >= target:
                print("  → Buddy_App 已点亮")
                break
    else:  # Buddy_App_QQ
        target = 1
        event = a.code or QQ_DEFAULT_EVENT
        if not a.yes:
            st = tc.task_status(c, a.task)
            cur = ((st.get("progress") or {}).get("current", 0)) if st else 0
            print(f"  [dry-run] 将 accept + 上报 1 个 {event} 事件，"
                  f"elementId={QQ_ENTER_TEMPLATE_ID} ({QQ_APP_NAME}) (当前 {cur}/{target}，加 --yes 生效)")
            sys.exit(0)
        name = fetch_qq_application(c)
        run_one(c, a.task, event, QQ_ENTER_TEMPLATE_ID, name, 1, target)


if __name__ == "__main__":
    main()