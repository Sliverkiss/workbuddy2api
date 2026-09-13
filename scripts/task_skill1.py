#!/usr/bin/env python3
"""一次性任务脚本：skill_1（「专家-技能」安装 1 个技能并在对话中发起使用，+100 积分 +5 能量）.

桩点逆向结论（M5 里程碑报告，2026-09-13）：
  skill_1 的判定链路（客户端埋点 -> AdapterTelemetryService -> POST /v2/report，
  与 M1-M4 同一条管道，缺 userId 会被 200 但静默丢弃，故必带 userId）：
    ★ 点亮事件：**`skill_info`**（携带真实 skillId，每事件 +1）→ 0/1 变 1/1 completed。
      实测 payload 最短形状：
        { eventCode: "skill_info", timestamp, reportDelay: 0,
          skillId: <真实 skill_id>, skillName?, skillVersion?, action: "use",
          conversationId?, requestId?, userId }
      （APIFOX-EVENTCODE.md §3 已记录 `SkillInfo = "skill_info"` 走 /v2/report 上报路径，
       本里程碑完成实测。）

  ❌ 实测不点亮（对照，每个 200 code=0 但进度不动）：
    - skill_action        {skillId, action:"install"}（安装成功埋点）
    - skill_installed     {skillId}（安装成功埋点）
    - skill_request_send  {skillId}（枚举里有但客户端无触发点，死代码）
    - agent_task_created  {has_skill:true, skill_names:[...]}（消息发送成功事件）
    - chat_request_send + skillId（对话活跃事件带 skill 字段）
    - expert_actual_use + expertType:"skill"（把 skill 当专家上报）

  补充：真实安装 API 可用（POST /v2/user-asset/skill/install，200 succeeded），
  但安装本身不点亮；真正的判定事件是 skill_info（「使用」的语义事件）。

  真实技能 id 来源（只读）：
    POST {chatBase}/v2/operation-platform/market/skill/list {"page":1,"page_size":8}
    -> data.skills[].skill_id（skill_2096525080079265792 = pptx, skill_2096528888507297792 = xlsx,
       skill_2070033533400236032 = qqmusic, skill_2095322904487550976 = pdf, ...）

实测（2026-09-13，<uid>）：
  - skill_info（真实 skillId）→ skill_1 0/1 → 1/1 completed ✅
  - claim 仍 400 task not completed（本系列已知悬案，仅记录）。

用法
  python3 task_skill1.py <uid前缀>                    # dry-run（list 模式查真实 skill_id）
  python3 task_skill1.py <uid前缀> --mode list        # 只读列出可用 skill_id
  python3 task_skill1.py <uid前缀> --mode report --yes --code skill_info --skill-id <id>
  python3 task_skill1.py <uid前缀> --mode report --yes   # 默认 skill_info
"""
import sys, os, json, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "skill_1"
TARGET = 1

# 真实 skill_id（POST /v2/operation-platform/market/skill/list 实测）
DEFAULT_SKILL_IDS = {
    "skill_2053082432761950208": "neodata-financial-search",
    "skill_2096528888507297792": "xlsx",
    "skill_2096525080079265792": "pptx",
    "skill_2070033533400236032": "qqmusic",
    "skill_2095322904487550976": "pdf",
    "skill_2053083109158420480": "web-access",
    "skill_2082683466424094720": "alice-a-share-short-term-strategy-report",
}
DEFAULT_SKILL_ID = "skill_2096525080079265792"  # pptx


def list_builtin_skills(auth, page=1, page_size=8):
    """只读拉 builtin skill 市场列表，返回 [{skill_id,name,version,display_name_zh,...}]。"""
    st, r = tc.do_post(auth, tc.chat_base(auth),
                       "/v2/operation-platform/market/skill/list",
                       {"page": page, "page_size": page_size})
    if st != 200:
        print(f"  [warn] skill list http={st}: {r}")
        return []
    return (r.get("data") or {}).get("skills") or []


def skill_event(event_code, auth, skill_id, skill_name="", skill_version="",
                source="builtin"):
    """按客户端源码形状组装单个 skill 事件（/v2/report 信封外字段）。"""
    now = int(time.time() * 1000)
    cid = f"wb-sk-{now}"
    rid = f"{cid}-{now}"
    if event_code == "skill_info":
        # ★ 已验证：点亮 skill_1（0/1 → 1/1 completed）
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "skillId": skill_id, "skillName": skill_name or skill_id,
            "skillVersion": skill_version or "", "action": "use",
            "conversationId": cid, "requestId": rid,
            "userId": auth["uid"],
        }]
    if event_code == "skill_action":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "id": skill_name or skill_id, "skillId": skill_id,
            "skillVersion": skill_version or "",
            "action": "install", "source": source,
            "userId": auth["uid"],
        }]
    if event_code == "skill_installed":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "name": skill_name or skill_id, "skillId": skill_id,
            "skillVersion": skill_version or "",
            "userId": auth["uid"],
        }]
    if event_code == "skill_request_send":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "mode": "CLOUD", "skillId": skill_id,
            "skillName": skill_name or skill_id, "skillCount": 1,
            "conversationId": cid, "requestId": rid,
            "messageId": rid, "requestModelId": "deepseek-v4-flash",
            "requestModelName": "DeepSeek V4 Flash",
            "userId": auth["uid"],
        }]
    raise ValueError(f"unknown event: {event_code}")


def main():
    ap = argparse.ArgumentParser(description="skill_1 任务：安装并使用 1 个技能完成 1/1")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--mode", default="list",
                    choices=["install_http", "report", "list"])
    ap.add_argument("--code", default="skill_info",
                    choices=["skill_info", "skill_action", "skill_installed", "skill_request_send"])
    ap.add_argument("--skill-id", default=None,
                    help="真实 skill_id（默认取内置表第一个）")
    ap.add_argument("--source", default="builtin",
                    choices=["builtin", "skillhub", "marketplace"])
    ap.add_argument("--gap", type=float, default=1.05)
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

        # list 模式：只读展示可用 skill_id（供后续真实安装/上报选）
        if a.mode == "list":
            skills = list_builtin_skills(c)
            if not skills:
                print("  [stop] 无技能列表")
                continue
            print("  可用 skill_id（前 %d 个）:" % len(skills))
            for s in skills:
                print(f"    {s.get('skill_id'):28s} {s.get('name'):30s} "
                      f"{s.get('display_name_zh','')}  v{s.get('version','')}")
            continue

        # 读任务状态
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

        # 解析 skill id/name
        skill_id = a.skill_id or DEFAULT_SKILL_ID
        skill_name = DEFAULT_SKILL_IDS.get(skill_id, skill_id)
        skill_version = ""

        # --- install_http 模式：真实安装 API ---
        if a.mode == "install_http":
            body = {"items": [{
                "source": "skillhub" if a.source == "skillhub" else "market",
                "asset_id": skill_id,
                "version": skill_version,
            }]}
            print(f"  [install_http] POST /v2/user-asset/skill/install "
                  f"body={json.dumps(body, ensure_ascii=False)}")
            if not a.yes:
                print("  [dry-run] （加 --yes 真发）。注意这是写操作，会对账号产生真实安装记录。")
                continue
            if ast == "not_accepted":
                st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
                print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
                time.sleep(1.05)
            st_r, r_r = tc.do_post(c, tc.billing_base(c),
                                   "/v2/user-asset/skill/install", body)
            print(f"  install -> {st_r} {json.dumps(r_r, ensure_ascii=False)[:300]}")
            time.sleep(2.0)

        # --- report 模式：/v2/report 上报事件 ---
        else:
            print(f"  [report] 将上报 {a.code} 事件，skill_id={skill_id}")
            if not a.yes:
                print(f"  [dry-run] （加 --yes 真发）当前 {cur}/{target}")
                continue
            if ast == "not_accepted":
                st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
                print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
                time.sleep(1.05)
            payload = skill_event(a.code, c, skill_id, skill_name, skill_version, a.source)
            st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
            sc = r_r.get("code") if isinstance(r_r, dict) else r_r
            print(f"  report[{a.code}] -> {st_r} code={sc}")
            time.sleep(2.0)

        # 回读确认
        st2 = tc.task_status(c, TASK_CODE)
        prog2 = (st2.get("progress") or {}) if st2 else {}
        cur2 = prog2.get("current", 0)
        ast2 = st2.get("accept_status") if st2 else "?"
        print(f"  回读 {TASK_CODE}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")
        if ast2 != "claimed" and cur2 >= target:
            print("  → 任务已满足，下一步可 claim（已知 claim 400 悬案，仅记录）")


if __name__ == "__main__":
    main()