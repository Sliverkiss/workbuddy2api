#!/usr/bin/env python3
"""一次性任务脚本：expert_5（「专家」召唤 5 位不同的专家并发起对话，+100 积分 +5 能量）.

桩点逆向结论（M3 里程碑报告，2026-09-13）：
  expert_5 的判定链路（Eth 客户端埋点 -> AdapterTelemetryService -> POST /v2/report，
  与 M1 create_canvas / M2 template_5 同一条管道，缺 userId 会被 200 但静默丢弃，
  故必带 userId）：
    1. expert_summoned        —— 召唤成功上报（ui-docs-viewer performSummon 内
       reportEvent(Events.ExpertSummoned, {id, name, expertTitle, type})）。
       payload: { id: 真实专家 id, name, expertTitle, type: industry/categoryId }。
    2. expert_actual_use      —— 带该专家「发起对话（发送成功）」后上报
       （reportConversationExpertActualUse）。payload 形状 {mode, id, name,
       expertTitle, type, expertType, source, version, cost, ...requestId}。
       automation 保存路径 saveDraft 里也会报（见 automation-7Pylv4sM.js）。

  关键事实：事件里的 id = **真实专家 id**，来源是专家市场清单：
    https://acc-1258344699.cos.accelerate.myqcloud.com/workbuddy/expert-marketplace/expert_center.json
    -> data.experts[]?.id（ContentCreator, UiDesigner, DataAnalyticsReporter, ...）
  不是任务表的 template_id_fixed（Expert_lighthouse 才用到，这里是 expert_5 独立任务）。

  M2 模式推广：任务点亮 = 「发送成功后的事件」（语义=真实动作发生）+ 真实业务对象 id，
  每事件 +1。expert_5 描述为「召唤 5 位不同的专家并发起对话」，语义对
  expert_summoned（召唤成功）与 expert_actual_use（发起对话）都符合，本脚本以
  expert_summoned 为主试，expert_actual_use 可 --code 切换、--ids 配不同专家。

实测（2026-09-13，<uid>）：
  - accept 后按不同真实专家 id 上报，逐步 +1，见 (M3-expert_5).md。
  - claim 仍 400 task not completed（本系列已知悬案，仅记录）。

用法
  python3 task_expert5.py <uid前缀>                 # dry-run（打载荷，不发）
  python3 task_expert5.py <uid前缀> --yes           # accept + 5 个不同专家事件 + 回读
  python3 task_expert5.py <uid前缀> --yes --count 2 # 只发 2 个（验证机制）
  python3 task_expert5.py <uid前缀> --yes --code expert_actual_use
"""
import sys, os, json, time, argparse, urllib.request
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "expert_5"
TARGET = 5

DEFAULT_EVENT = "expert_summoned"

# 真实专家 id（COS 专家市场清单，只读 GET）
EXPERT_MARKET_URL = ("https://acc-1258344699.cos.accelerate.myqcloud.com/"
                     "workbuddy/expert-marketplace/expert_center.json")
# 内置专家缺省 id（与清单一致；运行时优先从清单拉，失败回落到这 8 个）
DEFAULT_EXPERT_IDS = [
    "ContentCreator", "UiDesigner", "DataAnalyticsReporter", "ChinaEcommerceOperationsExpert",
    "DouyinStrategist", "SalesCoach", "BrandGuardian", "XiaohongshuOperationsExpert",
]
EXPERT_NAMES = {
    "ContentCreator": "内容创作专家", "UiDesigner": "UI设计师",
    "DataAnalyticsReporter": "数据分析报告师", "ChinaEcommerceOperationsExpert": "中国电商运营专家",
    "DouyinStrategist": "抖音策略师", "SalesCoach": "销售教练",
    "BrandGuardian": "品牌策略师", "XiaohongshuOperationsExpert": "小红书运营专家",
}


def fetch_market_experts():
    """只读拉一次专家市场清单，返回 {id: {categoryId, profession_zh, expertType}}。"""
    out = {}
    try:
        req = urllib.request.Request(EXPERT_MARKET_URL,
                                     headers={"User-Agent": "Mozilla/5.0"})
        with urllib.request.urlopen(req, timeout=20) as r:
            data = json.loads(r.read().decode("utf-8", "replace"))
        for e in (data.get("experts") or []):
            eid = e.get("id")
            if not eid:
                continue
            prof = e.get("profession") or {}
            out[eid] = {
                "categoryId": e.get("categoryId") or "",
                "profession_zh": prof.get("zh") or "",
                "expertType": e.get("expertType") or "",
            }
    except Exception as ex:
        print(f"  [warn] 专家清单拉取失败({ex})，回落内置表")
    return out


def expert_event(event_code, auth, expert_id, expert_name="", category="",
                 expert_type="agent", source="builtin", version=""):
    """按客户端源码形状组装单个专家事件（/v2/report 信封外字段）。"""
    now = int(time.time() * 1000)
    cid = f"wb-exp-{now}"
    rid = f"{cid}-{now}"
    if event_code == "expert_summoned":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "id": expert_id, "name": expert_name or expert_id,
            "expertTitle": expert_name or "", "type": category or "all",
            "userId": auth["uid"],
        }]
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
    raise ValueError(f"unknown event: {event_code}")


def main():
    ap = argparse.ArgumentParser(description="expert_5 任务：N 个不同专家事件完成 N/5")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--code", default=DEFAULT_EVENT,
                    choices=["expert_summoned", "expert_actual_use"])
    ap.add_argument("--count", type=int, default=0,
                    help="本次专家数（默认补到 5）")
    ap.add_argument("--ids", default=None,
                    help="逗号分隔的真实专家 id（默认清单前 5 个以内置回退）")
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

        # 确定专家 id 集合：--ids 优先，否则市场清单，否则内置表
        market = fetch_market_experts()
        ids = []
        if a.ids:
            ids = [x.strip() for x in a.ids.split(",") if x.strip()]
        else:
            ordered = list(market.keys()) + \
                [i for i in DEFAULT_EXPERT_IDS if i not in market]
            ids = ordered[:need]
        # 去重（同 id 只发一次，5 个不同专家 = 5 个不同 id）
        seen, final = set(), []
        for i in ids:
            if i not in seen:
                seen.add(i)
                final.append(i)
        ids = final[:need]

        if not a.yes:
            print(f"  [dry-run] 将 accept + 上报 {need} 个 {a.code} 事件，"
                  f"专家 id={ids} (当前 {cur}/{target}，加 --yes 生效)")
            for eid in ids:
                meta = market.get(eid) or {}
                payload = expert_event(a.code, c, eid,
                                       meta.get("profession_zh") or EXPERT_NAMES.get(eid, ""),
                                       meta.get("categoryId", ""), meta.get("expertType", ""))
                print(f"    -> {json.dumps(payload, ensure_ascii=False)}")
            continue

        # 1. accept（任务进度从接单开始计）
        if ast == "not_accepted":
            st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
            print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
            time.sleep(1.05)

        # 2. 依次上报（不同专家 id，模拟 5 个不同专家）
        for i, eid in enumerate(ids):
            meta = market.get(eid) or {}
            payload = expert_event(a.code, c, eid,
                                   meta.get("profession_zh") or EXPERT_NAMES.get(eid, ""),
                                   meta.get("categoryId", ""), meta.get("expertType", ""))
            st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
            sc = r_r.get("code") if isinstance(r_r, dict) else r_r
            print(f"  report[{a.code} #{eid}] -> {st_r} code={sc}")
            if i < len(ids) - 1:
                time.sleep(a.gap)

        if a.code == "expert_actual_use":
            time.sleep(2.0)  # 实际使用时可能存在异步服务端归账
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