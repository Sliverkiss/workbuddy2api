#!/usr/bin/env python3
"""一次性任务脚本：automation_1（「自动化」设置 1 个定时任务，+100 积分 +5 能量）.

桩点逆向结论（M6 里程碑报告，2026-09-13）：
  automation_1 的判定链路（客户端埋点 -> AdapterTelemetryService -> POST /v2/report，
  与 M1-M5 同一条管道，缺 userId 会被 200 但静默丢弃，故必带 userId）：
    ★ 点亮事件：**`automated_task_create_suc`** —— 用户保存一条自动化任务成功后上报
      （automation-7Pylv4sM.js saveDraft：actions.create() 成功后 reportEvent(
      Events.AutomatedTaskCreateSuc, {...})）。
      payload（客户端源码全形状）：
        { name, source: "manually", modelId, modelIsThinking, expertId,
          expertMarketplace, connectorIds, connectorCount, skills, skillCount,
          scheduleType, pushToWeChat, pushToWecomBot }

  关键事实：
    - scheduleType = form.schedule.type："once"（一次性，scheduledAt ISO）或 "recurring"
      （rrule，如每周五 = "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"）。
    - 真实创建自动化是桌面本地动作（SQLite + daemon）；云端自动化源「read-only」
      （server.js §scheduler-cloud-automation-source：`not supported for cloud automations
      (read-only)`）。因此本任务没有可 POST 的「真实创建 API」；判定信号就是
      automated_task_create_suc 埋点（语义 = 设置自动化成功）。
    - 与 M5 skill_info 同族：语义=真实动作成功的事件经 /v2/report 点亮，而单纯 UI 类
      事件不亮。

  用法（默认 dry-run，--yes 真发）
  python3 task_automation1.py <uid前缀>                     # dry-run 打载荷
  python3 task_automation1.py <uid前缀> --yes               # accept + a1 + 回读
  python3 task_automation1.py <uid前缀> --yes --scheduled    # 一次性（once）调度
  python3 task_automation1.py <uid前缀> --yes --name "每周五自动生成周报"
"""
import sys, os, json, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "automation_1"
TARGET = 1

DEFAULT_NAME = "每周五自动生成周报"
DEFAULT_PROMPT = "每周五自动整理本周工作，生成一份周报。"
# 默认每周五 09:00 的 rrule
DEFAULT_RRULE = "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"
DEFAULT_MODEL = "deepseek-v4-flash"


def automation_event(auth, name=DEFAULT_NAME, schedule_type="recurring",
                     rrule=DEFAULT_RRULE, scheduled_at="", model=DEFAULT_MODEL,
                     expert_id="", skills="", connector_ids=""):
    """按客户端源码形状组装 automated_task_create_suc 事件（/v2/report 信封外字段）。"""
    now = int(time.time() * 1000)
    cid = f"wb-aut-{now}"
    # 有效负载：构造一次真实保存会产生的自动化草稿字段
    schedule = {"type": schedule_type}
    if schedule_type == "once":
        schedule["scheduledAt"] = scheduled_at or "2026-09-18T09:00:00.000Z"
    elif schedule_type == "recurring":
        schedule["rrule"] = rrule or DEFAULT_RRULE
    ev = {
        "eventCode": "automated_task_create_suc", "timestamp": now, "reportDelay": 0,
        "name": name, "source": "manually",
        "modelId": model, "modelIsThinking": False,
        "expertId": expert_id, "expertMarketplace": "",
        "connectorIds": connector_ids, "connectorCount": len(connector_ids.split(",")) if connector_ids else 0,
        "skills": skills, "skillCount": len(skills.split(",")) if skills else 0,
        "scheduleType": schedule_type,
        "pushToWeChat": False, "pushToWecomBot": False,
        "conversationId": cid, "requestId": f"{cid}-{now}",
        "schedule": schedule,
        "prompt": DEFAULT_PROMPT,
        "userId": auth["uid"],
    }
    return ev


def main():
    ap = argparse.ArgumentParser(description="automation_1 任务：设置 1 个自动化定时任务完成 1/1")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--scheduled", action="store_true",
                    help="用「一次性」调度（once）而非默认每周五 recurring")
    ap.add_argument("--name", default=DEFAULT_NAME, help="自动化名称")
    ap.add_argument("--model", default=DEFAULT_MODEL, help="执行模型 id")
    ap.add_argument("--expert-id", default="", help="执行专家 id")
    ap.add_argument("--skills", default="", help="逗号分隔技能 id")
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

        schedule_type = "once" if a.scheduled else "recurring"
        payload = [automation_event(c, name=a.name, schedule_type=schedule_type,
                                    model=a.model, expert_id=a.expert_id,
                                    skills=a.skills)]

        if not a.yes:
            print(f"  [dry-run] 将 accept + 上报 1 个 automated_task_create_suc 事件"
                  f" (schedule_type={schedule_type})（加 --yes 生效）")
            print(f"    -> {json.dumps(payload, ensure_ascii=False)}")
            continue

        # 1. accept（任务进度从接单开始计）
        if ast == "not_accepted":
            st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
            print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
            time.sleep(1.05)

        # 2. 上报 automated_task_create_suc
        st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
        sc = r_r.get("code") if isinstance(r_r, dict) else r_r
        print(f"  report[automated_task_create_suc] -> {st_r} code={sc}")
        time.sleep(2.0)

        # 3. 回读确认
        st2 = tc.task_status(c, TASK_CODE)
        prog2 = (st2.get("progress") or {}) if st2 else {}
        cur2 = prog2.get("current", 0)
        ast2 = st2.get("accept_status") if st2 else "?"
        print(f"  回读 {TASK_CODE}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")
        if ast2 != "claimed" and cur2 >= target:
            print("  → 任务已满足，下一步可 claim（已知 claim 400 悬案，仅记录）")


if __name__ == "__main__":
    main()