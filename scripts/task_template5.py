#!/usr/bin/env python3
"""一次性任务脚本：template_5（使用 5 个不同的模板并发起对话，+100 积分 +5 能量）.

桩点逆向结论（M2 里程碑报告，2026-09-13）：
  template_5 的判定链路（两条都经 AdapterTelemetryService -> POST /v2/report，与
  M1 create_canvas 画布埋点同链路，缺 userId 会被 200 但静默丢弃，故必带 userId）：
    1. template_used                 —— 点击「输入框上方模板」时上报（home-BECKZUzd.js
       reportTemplateUsed；ui-docs-viewer useQuickActionsConfig.handleActionClick）。
       payload: { template_id, template_name?, task_mode, mode?, prompt_index? }。
    2. agent_task_created_with_template —— 发送成功后上报（main-content-core 发送回包
       reportEvent$1 分支）。payload 形状 { isCustomModel, id, name, requestId }。

  关键事实：template_id = String(template.id) 是【场景 id】，来自
    GET /console/as/support/scenes?locale=zh-CN  -> data.scenes[].id（0,2,4,...,22,...）
  真实场景 id（幻灯片=0、视频生成=2、深度研究=4、文档处理=6、数据分析=8、可视化=10、
  金融服务=12、产品管理=14、设计=16、邮件编辑=18、日常开发=20、网站开发=22, ...）。
  不是任务表的 template_id_fixed（M0.5 用 id=22(=template_id_fixed) 走 chat extra_vars
  通道不亮——错对象 + 错通道；/v2/report 的 template_used 通道未测过）。

实测（2026-09-13，<uid>）：
  - accept 后 5 个不同场景 id 的 agent_task_created_with_template（经 /v2/report）
    逐步点亮：0/5 -> 1/5(id=2) -> 2/5(id=4) -> ... -> 5/5 completed。每事件 +1。
  - template_used 事件单独上报 200 code=0 但【不点亮】（0/5 不变）。
  - M0.5 失败根因：用的是 template_id_fixed（22）且走 chat extra_vars 通道；
    正确 = 真实场景 id（/console/as/support/scenes）+ /v2/report 通道。
  - claim 仍 400 task not completed（已知悬案，仅记录不深挖）。

用法
  python3 task_template5.py <uid前缀>                  # dry-run（打载荷，不发）
  python3 task_template5.py <uid前缀> --yes            # accept + 5 个不同模板事件 + 回读
  python3 task_template5.py <uid前缀> --yes --count 2  # 只发 2 个模板（验证机制）
  python3 task_template5.py <uid前缀> --yes --code agent_task_created_with_template
"""
import sys, os, json, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "template_5"
TARGET = 5

DEFAULT_EVENT = "template_used"

# 真实场景 id（GET /console/as/support/scenes -> data.scenes[].id）
DEFAULT_TEMPLATE_IDS = [0, 2, 4, 6, 8]
SCENE_NAMES = {
    0: "幻灯片", 2: "视频生成", 4: "深度研究", 6: "文档处理",
    8: "数据分析", 10: "可视化", 12: "金融服务", 14: "产品管理",
    16: "设计", 18: "邮件编辑", 20: "日常开发", 22: "网站开发",
}


def template_event(event_code, auth, scene_id, scene_name, prompt_index=None):
    """按客户端源码形状组装单个模板事件（/v2/report 信封外字段）。"""
    now = int(time.time() * 1000)
    cid = f"wb-tpl-{now}"
    rid = f"{cid}-{now}"
    tid = str(scene_id)
    if event_code == "template_used":
        ev = {
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "template_id": tid, "task_mode": "working",
            "mode": "work", "conversationId": cid,
        }
        # 客户端有时带 prompt_index / template_name，可选项对齐
        if prompt_index is not None:
            ev["prompt_index"] = prompt_index
        if scene_name:
            ev["template_name"] = scene_name
        ev["userId"] = auth["uid"]
        return [ev]
    if event_code == "agent_task_created_with_template":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "isCustomModel": True, "id": tid, "name": scene_name or "",
            "requestId": rid, "conversationId": cid, "userId": auth["uid"],
        }]
    raise ValueError(f"unknown event: {event_code}")


def fetch_scene_names(auth):
    """尝试拉一次真实场景 id->名称 映射（只读）。失败返回内置表。"""
    try:
        st, d = tc.do_get(auth, tc.chat_base(auth), "/console/as/support/scenes?locale=zh-CN")
        if st == 200 and (d.get("code") in (0, None)):
            data = d.get("data") or {}
            scenes = data.get("scenes") or []
            names = {}
            for s in scenes:
                if s.get("id") is not None:
                    names[str(s["id"])] = s.get("name") or ""
            if names:
                return names
    except Exception:
        pass
    return SCENE_NAMES


def main():
    ap = argparse.ArgumentParser(description="template_5 任务：N 个不同场景模板事件完成 N/5")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--code", default=DEFAULT_EVENT,
                    choices=["template_used", "agent_task_created_with_template"])
    ap.add_argument("--count", type=int, default=0,
                    help="本次模板数（默认补到 5）")
    ap.add_argument("--ids", default=None,
                    help="逗号分隔的场景 id（默认从 scenes API/内置表取 5 个）")
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

        need = max(0, target - cur)
        if a.count > 0:
            need = min(need, a.count)
        if need <= 0:
            print("  [skip] 无需上报")
            continue

        # 确定场景 id 集合：--ids 优先，否则 scenes API，否则内置表
        scene_names = fetch_scene_names(c)
        ids = []
        if a.ids:
            ids = [x.strip() for x in a.ids.split(",") if x.strip()]
        else:
            ids = [i for i in (DEFAULT_TEMPLATE_IDS + list(range(20, 120, 2)))
                   if str(i) in scene_names or True][:need]
            # 用内置表优先保证是已知场景；none 兜底连续 id
        # 去重（同 id 只发一次，5 个不同模板 = 5 个不同 id）
        seen, final = set(), []
        for i in ids:
            if i not in seen:
                seen.add(i)
                final.append(i)
        ids = final[:need]

        if not a.yes:
            print(f"  [dry-run] 将 accept + 上报 {need} 个 {a.code} 事件，"
                  f"场景 id={ids} (当前 {cur}/{target}，加 --yes 生效)")
            for i in ids:
                nm = scene_names.get(str(i)) or SCENE_NAMES.get(int(i), "")
                payload = template_event(a.code, c, int(i) if str(i).isdigit() else i, nm)
                print(f"    -> {json.dumps(payload, ensure_ascii=False)}")
            continue

        # 1. accept（任务进度从接单开始计）
        if ast == "not_accepted":
            st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
            print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
            time.sleep(1.05)

        # 2. 依次上报（不同场景 id，模拟 5 个不同模板）
        for i, sid in enumerate(ids):
            nm = scene_names.get(str(sid)) or SCENE_NAMES.get(int(sid), "")
            payload = template_event(a.code, c, int(sid) if str(sid).isdigit() else sid, nm,
                                     prompt_index=i)
            st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
            sc = r_r.get("code") if isinstance(r_r, dict) else r_r
            print(f"  report[{a.code} #{sid}] -> {st_r} code={sc}")
            if i < len(ids) - 1:
                time.sleep(a.gap)

        time.sleep(1.0)

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