#!/usr/bin/env python3
"""一次性任务脚本：playbook_prompt（「探索优秀灵感」完成 1 次对话，+100 积分 +5 能量）.

桩点逆向结论（M7 里程碑报告，2026-09-13）：
  playbook_prompt 判定链路（Eth 客户端埋点 -> AdapterTelemetryService -> POST /v2/report，
  与 M1-M6 同一条管道，缺 userId 会被 200 但静默丢弃，故必带 userId）：
    ★ 点亮事件：**`playbook_prompt_send`** —— 「做同款」装载案例并【发送成功】后上报
      （main-content-core 发送回包 sendAccepted 分支，pendingPlaybookMetaRef 转
      reportEvent$1(Events.PlaybookPromptSend)）。语义 = 真实动作发生 + 真实案例 id。

  payload 形状（照抄主 bundle 发送成功分支，全字段）：
    { eventCode:"playbook_prompt_send", id:<案例id>, name:案例标题, type:artifact_type,
      promptLength:<prompt 长度>, isOfficial:1,
      skills: 案例技能 id 逗号串, skillNames: 技能名逗号串,
      expertId: 案例关联专家 id, expertName: 专家名,
      categoryId: 案例分类, categoryName: 分类名, query: searchKeyword,
      source: "discover"|"home"|"scene"|"search"|"featured"|"favorite",
      conversationId, requestId, ext1: "home"|"discover", userId }

  案例(playbook) id 来源 = 灵感案例注册表（静态 COS，只读 GET，无需鉴权）：
    https://static.workbuddy.cn/workbuddy/playbook/registry.json
      -> data.cases[]?.id（当前 1053 个，如 worker-ledger-freedom-dashboard = 打工人小账本）
  分类名来源 categories.json -> data.categories[].id/name。
  案例详情来源 /cases/<id>/case.json（选填，含 prompt/skills/experts 供 payload 对齐）。

  ❌ 浏览类不亮（对照，M7 实测）：playbook_cta_click（点击「制作我的版本」）、
  WebPageShow(playbook_list/playbook_detail)、web_element_click —— 点击不构成「完成对话」。

实测（2026-09-13，<uid>）：
  - accept 后上报 1 次 playbook_prompt_send（真实案例 id）→ 0/1 变 1/1 completed。
  - claim 仍 400 task not completed（本系列已知悬案，仅记录）。

用法
  python3 task_playbook_prompt.py <uid前缀>              # dry-run（打载荷，不发）
  python3 task_playbook_prompt.py <uid前缀> --yes        # accept + 1 事件 + 回读
  python3 task_playbook_prompt.py <uid前缀> --yes --case-id <id>
  python3 task_playbook_prompt.py <uid前缀> --yes --code playbook_cta_click   # 对照
"""
import sys, os, json, time, argparse, urllib.request
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "playbook_prompt"
TARGET = 1

DEFAULT_EVENT = "playbook_prompt_send"

PLAYBOOK_BASE = "https://static.workbuddy.cn/workbuddy/playbook"
# 内置兜底案例 id（registry.json 缺省，运行时优先拉注册表）
DEFAULT_CASE_ID = "worker-ledger-freedom-dashboard"


def fetch_registry():
    """只读拉一次灵感案例注册表，返回 {case_id: {title, artifact_type, categories, skills, experts, prompt}}。"""
    out = {}
    try:
        req = urllib.request.Request(f"{PLAYBOOK_BASE}/registry.json",
                                     headers={"User-Agent": "Mozilla/5.0"})
        with urllib.request.urlopen(req, timeout=20) as r:
            data = json.loads(r.read().decode("utf-8", "replace"))
        for c in (data.get("cases") or []):
            cid = c.get("id")
            if not cid:
                continue
            out[cid] = {
                "title": c.get("title") or "",
                "artifact_type": c.get("artifact_type") or "",
                "categories": c.get("categories") or [],
                "skills": c.get("skills") or [],
                "experts": c.get("experts") or [],
                "prompt": c.get("prompt") or "",
            }
    except Exception as ex:
        print(f"  [warn] 案例注册表拉取失败({ex})，回落内置表")
    return out


def fetch_case_detail(case_id):
    """只读拉单个案例详情（选填，供 prompt/skills/experts 精确对齐）。失败返回 None。"""
    try:
        req = urllib.request.Request(f"{PLAYBOOK_BASE}/cases/{case_id}/case.json",
                                     headers={"User-Agent": "Mozilla/5.0"})
        with urllib.request.urlopen(req, timeout=20) as r:
            return json.loads(r.read().decode("utf-8", "replace"))
    except Exception:
        return None


def fetch_categories():
    """只读拉分类清单，返回 {id: name}。失败返回空。"""
    out = {}
    try:
        req = urllib.request.Request(f"{PLAYBOOK_BASE}/categories.json",
                                     headers={"User-Agent": "Mozilla/5.0"})
        with urllib.request.urlopen(req, timeout=20) as r:
            data = json.loads(r.read().decode("utf-8", "replace"))
        for c in (data.get("categories") or []):
            if c.get("id"):
                out[c["id"]] = c.get("name") or ""
    except Exception:
        pass
    return out


def pick_expert_name(experts, locale="zh"):
    """照抄客户端 pickExpertName：返回专家展示名（zh 优先）。"""
    if not experts:
        return None
    e = experts[0]
    nm = e.get("name") or e.get("expert_id") or e.get("id") or None
    if isinstance(nm, dict):
        return nm.get(locale) or nm.get("en") or nm.get("zh")
    return nm


def playbook_event(event_code, auth, case_id, case=None, case_name="",
                   categories=None, source="discover", query=""):
    """按客户端源码形状组装单个 playbook 事件（/v2/report 信封外字段）。"""
    now = int(time.time() * 1000)
    cid = f"wb-pb-{now}"
    rid = f"{cid}-{now}"
    case = case or {}
    if event_code == "playbook_prompt_send":
        skills = case.get("skills") or []
        experts = case.get("experts") or []
        category_id = (case.get("categories") or [""])[0] or ""
        category_name = (categories or {}).get(category_id, "")
        prompt = case.get("prompt") or ""
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "id": case_id,
            "name": case_name or case.get("title") or case_id,
            "type": case.get("artifact_type") or "other",
            "promptLength": len(prompt),
            "isOfficial": 1,
            "skills": ",".join(s.get("skill_id", "") for s in skills if s.get("skill_id")),
            "skillNames": ",".join(s.get("name", "") for s in skills if s.get("name")),
            "expertId": (experts[0].get("expert_id") or experts[0].get("id")) if experts else None,
            "expertName": (pick_expert_name(experts) or "") if experts else "",
            "categoryId": category_id,
            "categoryName": category_name,
            "query": query,
            "source": source,
            "conversationId": cid, "requestId": rid,
            "ext1": "discover" if source != "home" else "home",
            "userId": auth["uid"],
        }]
    if event_code == "playbook_cta_click":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "id": case_id, "name": case_name or case_id,
            "type": (case or {}).get("artifact_type") or "other",
            "source": source, "position": 1, "userId": auth["uid"],
        }]
    raise ValueError(f"unknown event: {event_code}")


def main():
    ap = argparse.ArgumentParser(description="playbook_prompt 任务：1 次「做同款」发送完成 1/1")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--code", default=DEFAULT_EVENT,
                    choices=["playbook_prompt_send", "playbook_cta_click"])
    ap.add_argument("--case-id", default=None, help="真实灵感案例 id（默认注册表第 1 个）")
    ap.add_argument("--source", default="discover",
                    choices=["discover", "home", "scene", "search", "featured", "favorite"])
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

        # 确定案例 id + 元数据：--case-id 优先，否则注册表，否则内置兜底
        registry = fetch_registry()
        categories = fetch_categories()
        case_id = a.case_id or (next(iter(registry)) if registry else DEFAULT_CASE_ID)
        case_meta = registry.get(case_id) or {}
        if not case_meta:
            # 没在注册表里（用户手填 id），尝试拉详情
            detail = fetch_case_detail(case_id)
            if detail:
                case_meta = {
                    "title": detail.get("title") or "",
                    "artifact_type": detail.get("artifact_type") or "",
                    "categories": detail.get("categories") or [],
                    "skills": detail.get("skills") or [],
                    "experts": detail.get("experts") or [],
                    "prompt": detail.get("prompt") or "",
                }

        if not a.yes:
            print(f"  [dry-run] 将 accept + 上报 1 个 {a.code} 事件，"
                  f"case_id={case_id} (当前 {cur}/{target}，加 --yes 生效)")
            payload = playbook_event(a.code, c, case_id, case_meta,
                                     case_meta.get("title", ""), categories, a.source)
            print(f"    -> {json.dumps(payload, ensure_ascii=False)}")
            continue

        # 1. accept（任务进度从接单开始计）
        if ast == "not_accepted":
            st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
            print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
            time.sleep(1.05)

        # 2. 上报 1 次（真实案例 id；playbook_prompt_send =「发送成功」语义事件）
        payload = playbook_event(a.code, c, case_id, case_meta,
                                 case_meta.get("title", ""), categories, a.source)
        st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
        sc = r_r.get("code") if isinstance(r_r, dict) else r_r
        print(f"  report[{a.code} #{case_id}] -> {st_r} code={sc}")

        if a.code == "playbook_prompt_send":
            time.sleep(2.0)  # 服务端归账可能异步
        else:
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