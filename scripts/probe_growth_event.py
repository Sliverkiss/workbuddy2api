#!/usr/bin/env python3
"""M0 攻坚探针：growthEvent 权威完成通道验证（真实 chat 带 extra_vars.growthEvent）.

假设（方向 A 静态结论，2026-09-13）：
  - 成长任务判定存在「权威完成记录」。template_id_fixed>0 的任务（RichMeow_Chat=11、
    Expert_Philanthropy=22、Hp_Appearance=23、black_cat=10）在桌面端由客户端把
    growthEvent（agent_task_created_with_template）挂在真实 chat/completions 请求体
    顶层的 extra_vars.growthEvent 里上行 —— 这是与 /v2/report 埋点独立的权威通道。
  - create_canvas template_id_fixed=0，不走 growthEvent，疑似靠「真实画布创建」的业务
    副作用 API。本脚本聚焦验证「带 growthEvent 的真实 chat 是否生成权威完成记录并可 claim」。

流程（每个 ——yes 只对 1 个账号做 1 次真实 chat + 回读）：
  1. 读任务列表
  2. 选目标任务（默认取第一个 template_id_fixed>0 且非 claimed/completed 的任务）
  3. [--yes] accept（若 not_accepted）
  4. [--yes] 真实 chat/completions 一次，body 顶层 extra_vars.growthEvent 带目标事件码
  5. 回读进度 + 尝试 claim（观察 claim 是否从 400 task not completed 变为 200）

用法
  python3 probe_growth_event.py <uid>                    # dry-run 展示载荷
  python3 probe_growth_event.py <uid> --yes --template Expert_Philanthropy  # 选定 template
  python3 probe_growth_event.py <uid> --yes --code agent_task_created_with_template
"""
import sys, os, time, json, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

GROWTH_EVENT_CODES = [
    "agent_task_created_with_template",
    "agent_task_created",
    "expert_actual_use",
    "automated_task_create_suc",
]


def pick_template_task(auth, want_code=None, skip_claimed=True):
    """从任务列表里挑一个 template_id_fixed>0 的目标任务。"""
    tasks = tc.list_tasks(auth)
    for t in tasks:
        if t.get("template_id_fixed", 0) <= 0:
            continue
        if want_code and t.get("task_code") != want_code:
            continue
        ast = t.get("accept_status")
        prog = t.get("progress") or {}
        cur = prog.get("current", 0)
        target = prog.get("target", 1)
        if skip_claimed and (ast == "claimed" or cur >= target):
            continue
        return t
    # 兜底：再扫全部任务找一个非完成的
    for t in tasks:
        if want_code and t.get("task_code") != want_code:
            continue
        ast = t.get("accept_status")
        prog = t.get("progress") or {}
        cur = prog.get("current", 0)
        target = prog.get("target", 1)
        if skip_claimed and (ast == "claimed" or cur >= target):
            continue
        return t
    return None


def build_growth_event(ev_code, task):
    """按客户端 appendGrowthEvent 形状组装 growthEvent 条目。"""
    e = {"eventCode": ev_code}
    templ = task.get("template_id_fixed") or 0
    if ev_code == "agent_task_created_with_template":
        e["id"] = str(templ)
        e["extra"] = {"isCustomModel": True, "name": task.get("title") or ""}
    elif ev_code == "expert_actual_use":
        e["id"] = str(templ)
        e["extra"] = {"name": task.get("title") or "", "type": "", "source": ""}
    else:
        e["id"] = ""
        e["extra"] = {}
    return e


def main():
    ap = argparse.ArgumentParser(description="growthEvent 权威完成通道探针")
    ap.add_argument("account", help="uid 前缀")
    ap.add_argument("--yes", action="store_true")
    ap.add_argument("--template", default=None, help="目标 task_code（默认自动选）")
    ap.add_argument("--code", default=GROWTH_EVENT_CODES[0],
                    choices=GROWTH_EVENT_CODES, help="growthEvent 事件码")
    ap.add_argument("--prompt", default="图片设计相关的一句话，谢谢。", help="真实对话提示词")
    a = ap.parse_args()

    c = tc.load_auth(a.account)
    print(f"== {c['uid'][:8]} ({c['nick']}) ==")

    task = pick_template_task(c, want_code=a.template)
    if task is None:
        print("  [stop] 没有可用的 template 任务（全部 claimed / completed）")
        return
    code = task["task_code"]
    templ = task.get("template_id_fixed", 0)
    ast = task.get("accept_status")
    prog = task.get("progress") or {}
    print(f"  目标: {code} (template_id_fixed={templ}) accept_status={ast} "
          f"progress={prog.get('current', 0)}/{prog.get('target', 1)}")

    ge = build_growth_event(a.code, task)
    extra_var = {"growthEvent": json.dumps([ge])}
    print(f"  生长事件: {a.code} -> {json.dumps(ge, ensure_ascii=False)}")
    print(f"  extra_vars: {json.dumps(extra_var, ensure_ascii=False)}")

    if not a.yes:
        print("  [dry-run] 将 accept + 真实 chat 一次(带 extra_vars.growthEvent) + 回读"
              " + claim 尝试（加 --yes 生效）")
        return

    # 1. accept（若 not_accepted）
    if ast == "not_accepted":
        st_a, r_a = tc.accept_tasks(c, [code])
        print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
        time.sleep(1.05)

    # 2. 真实 chat 一次，带 growthEvent
    st_chat, first = tc.chat_completion(c, model_id="deepseek-v4-flash",
                                        prompt=a.prompt, extra_var=extra_var)
    print(f"  chat   -> {st_chat} {first[:50]!r}")

    # 3. 回读（事件计数可能在有 chat 回包后异步生效，稍等再回读）
    time.sleep(2.0)
    st2 = tc.task_status(c, code)
    prog2 = (st2.get("progress") or {}) if st2 else {}
    cur2 = prog2.get("current", 0)
    ast2 = st2.get("accept_status") if st2 else "?"
    print(f"  回读   -> {code}: {cur2}/{prog2.get('target', 1)} accept_status={ast2}")

    # 4. claim 尝试（不论进度，试一发观察判定变化）
    st_c, r_c = tc.claim_reward(c, code)
    msg = r_c.get("msg") if isinstance(r_c, dict) else r_c
    print(f"  claim  -> {st_c} {msg}")


if __name__ == "__main__":
    main()