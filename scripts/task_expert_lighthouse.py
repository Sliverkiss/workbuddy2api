#!/usr/bin/env python3
"""一次性任务脚本：Expert_lighthouse（「腾讯轻量云专家」发起对话 1 次，+100 积分）.

桩点逆向结论（M9 里程碑，2026-09-13）：
  Expert_lighthouse 与 M3 expert_5 同族（template_id_fixed=0, reward_buddy=false 的
  普通专家任务）—— 判定链路 = `expert_actual_use` 经 POST {billing}/v2/report，
  语义=「带真实专家发起对话（发送成功）」，每事件 +1，缺 userId 会被 200 静默丢弃。

  真实专家 id 权威源 = 运行时市场 API（chat 域）：
    POST {chatBase}/v2/operation-platform/market/expert/list
      {"page":1,"page_size":5,"keyword":"lighthouse"|"轻量云"}
      -> data.experts[] 命中 **ex_2cvvUZQhDyeJ**（agent_name=lighthouse-ops，
         display_name_zh=「腾讯轻量云专家」，version=1.0.2，categories=["02-Engineering"]）。
  不用 COS 静态清单（M8 教训：COS 是非运行时 id，客户端实际用 ex_ 前缀运行时 id）。

实测（2026-09-13，<uid>）：
  - accept 后上报 1 次 expert_actual_use（真实 ex_ id）→ 0/1 变 1/1 completed。
  - claim 仍 400 task not completed（本系列已知悬案，仅记录）。

用法
  python3 task_expert_lighthouse.py <uid前缀>            # dry-run（打载荷，不发）
  python3 task_expert_lighthouse.py <uid前缀> --yes      # accept + 1 事件 + 回读
  python3 task_expert_lighthouse.py <uid前缀> --yes --code expert_summoned   # 对照
"""
import sys, os, json, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "Expert_lighthouse"
TARGET = 1

DEFAULT_EVENT = "expert_actual_use"

MARKET_LIST_PATH = "/v2/operation-platform/market/expert/list"
# 运行时市场 API 关键词搜索命中（权威运行时 id）
DEFAULT_EXPERT_IDS = ["ex_2cvvUZQhDyeJ"]   # lighthouse-ops = 腾讯轻量云专家
EXPERT_NAMES = {
    "ex_2cvvUZQhDyeJ": "腾讯轻量云专家",
    "lighthouse-ops": "腾讯轻量云专家",
}
KEYWORDS = ["lighthouse", "轻量云"]


def fetch_lighthouse_experts(auth):
    """只读拉运行时专家市场清单，关键词过滤，返回 {expert_id: {categoryId, profession_zh, expertType, version}}。"""
    out = {}
    try:
        for kw in KEYWORDS:
            st, r = tc.do_post(auth, tc.chat_base(auth), MARKET_LIST_PATH,
                               {"page": 1, "page_size": 5, "keyword": kw})
            if st != 200:
                continue
            for e in (r.get("data") or {}).get("experts") or []:
                blob = json.dumps(e, ensure_ascii=False).lower()
                if not any(k in blob for k in ["lighthouse", "轻量云", "light-ops", "lighthouse-ops"]):
                    continue
                eid = e.get("expert_id") or e.get("source_id")
                if not eid:
                    continue
                out[eid] = {
                    "categoryId": (e.get("categories") or [""])[0] or "",
                    "profession_zh": e.get("profession_zh") or e.get("display_name_zh") or "",
                    "expertType": e.get("expert_type") or "agent",
                    "version": e.get("version") or "",
                }
    except Exception as ex:
        print(f"  [warn] 市场清单拉取失败({ex})，回落内置表")
    if not out:
        out = {DEFAULT_EXPERT_IDS[0]: {
            "categoryId": "02-Engineering", "profession_zh": "腾讯轻量云专家",
            "expertType": "agent", "version": "1.0.2"}}
    return out


def expert_event(event_code, auth, expert_id, expert_name="", category="",
                 expert_type="agent", source="builtin", version=""):
    """按客户端源码形状组装单个专家事件（/v2/report 信封外字段）。"""
    now = int(time.time() * 1000)
    cid = f"wb-lh-{now}"
    rid = f"{cid}-{now}"
    if event_code == "expert_actual_use":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "mode": "CLOUD",
            "id": expert_id, "name": expert_name or expert_id,
            "expertTitle": expert_name or "", "type": category or "",
            "expertType": expert_type or "agent", "source": source,
            "version": version or "", "cost": 0, "characterCount": 12,
            "conversationId": cid, "requestId": rid,
            "messageId": rid, "requestModelId": "deepseek-v4-flash",
            "requestModelName": "DeepSeek V4 Flash",
            "userId": auth["uid"],
        }]
    if event_code == "expert_summoned":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "id": expert_id, "name": expert_name or expert_id,
            "expertTitle": expert_name or "", "type": category or "all",
            "userId": auth["uid"],
        }]
    raise ValueError(f"unknown event: {event_code}")


def main():
    ap = argparse.ArgumentParser(description="Expert_lighthouse 任务：轻量云专家使用事件完成 1/1")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--code", default=DEFAULT_EVENT,
                    choices=["expert_actual_use", "expert_summoned"])
    ap.add_argument("--expert-id", default=None,
                    help="真实轻量云专家 id（默认市场清单命中的 ex_2cvvUZQhDyeJ）")
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

        market = fetch_lighthouse_experts(c)
        expert_id = a.expert_id or (next(iter(market)) if market else DEFAULT_EXPERT_IDS[0])
        meta = market.get(expert_id) or {}
        expert_name = meta.get("profession_zh") or EXPERT_NAMES.get(expert_id, "")
        category = meta.get("categoryId", "")
        expert_type = meta.get("expertType") or "agent"
        version = meta.get("version", "")

        if not a.yes:
            print(f"  [dry-run] 将 accept + 上报 1 个 {a.code} 事件，"
                  f"expert_id={expert_id} ({expert_name}) (当前 {cur}/{target}，加 --yes 生效)")
            payload = expert_event(a.code, c, expert_id, expert_name, category,
                                   expert_type, version=version)
            print(f"    -> {json.dumps(payload, ensure_ascii=False)}")
            continue

        if ast == "not_accepted":
            st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
            print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
            time.sleep(1.05)

        payload = expert_event(a.code, c, expert_id, expert_name, category,
                               expert_type, version=version)
        st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
        sc = r_r.get("code") if isinstance(r_r, dict) else r_r
        print(f"  report[{a.code} #{expert_id}] -> {st_r} code={sc}")

        if a.code == "expert_actual_use":
            time.sleep(2.0)  # 实际使用时可能存在异步服务端归账
        else:
            time.sleep(1.0)

        st2 = tc.task_status(c, TASK_CODE)
        prog2 = (st2.get("progress") or {}) if st2 else {}
        cur2 = prog2.get("current", 0)
        ast2 = st2.get("accept_status") if st2 else "?"
        print(f"  回读 {TASK_CODE}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")
        if ast2 != "claimed" and cur2 >= target:
            print("  → 任务已满足，下一步可 claim（已知 claim 400 悬案，仅记录）")


if __name__ == "__main__":
    main()