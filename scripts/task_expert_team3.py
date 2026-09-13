#!/usr/bin/env python3
"""一次性任务脚本：Expert_team_use_3（「专家团」召唤并使用 3 个不同的专家团队，+100 积分 +5 能量）.

桩点逆向结论（M4 里程碑报告，2026-09-13）：
  Expert_team_use_3 的判定链路（Eth 客户端埋点 -> AdapterTelemetryService -> POST /v2/report，
  与 M1 create_canvas / M2 template_5 / M3 expert_5 同一条管道，缺 userId 会被 200 但
  静默丢弃，故必带 userId）：
    1. expert_team_summon    —— 专家团队卡片「立即召唤」点击（ui-docs-viewer 的
       data-track-id: "expert_team_summon"）—— 点击观测，参照 M3 expert_summoned 大概率不亮。
    2. expert_actual_use     —— 团队被真实使用（发起对话/保存自动化含该团队 expertId）时
       上报（reportConversationExpertActualUse / saveDraft）。
       payload 形状 {mode, id, name, expertTitle, type, expertType, source, version, cost, ...}。
       `expertType: "team"`（团队专家）区分于普通 agent 专家。

  关键事实：事件里的 id = **真实专家团队 id**，来源专家市场清单（COS）：
    https://acc-1258344699.cos.accelerate.myqcloud.com/workbuddy/expert-marketplace/expert_center.json
    -> data.experts[]?.filter(expertType === "team").id
    （CloudOpsTeam, SoftwareCompany, TradingAgentTeam, GPTResearcherTeam, ... 共 52 个 team 专家）

  M3 实证：expert_actual_use（=「发送成功后的事件」）点亮 expert_5 0/5→5/5，而
  expert_summoned（点击）不亮。M4 同构：用 expert_actual_use + 3 个不同 team id 点亮
  3/3；expert_team_summon 留作对照（--code 可切）。

实测（2026-09-13，<uid>）：
  - accept 后按不同真实 team id 上报 expert_actual_use，逐步 +1，见 (M4-Expert_team_use_3).md。
  - claim 仍 400 task not completed（本系列已知悬案，仅记录）。

用法
  python3 task_expert_team3.py <uid前缀>                 # dry-run
  python3 task_expert_team3.py <uid前缀> --yes           # accept + 3 个不同团队事件 + 回读
  python3 task_expert_team3.py <uid前缀> --yes --count 1 # 只发 1 个（验证机制）
  python3 task_expert_team3.py <uid前缀> --yes --code expert_team_summon
"""
import sys, os, json, time, argparse, urllib.request
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "Expert_team_use_3"
TARGET = 3

DEFAULT_EVENT = "expert_actual_use"

EXPERT_MARKET_URL = ("https://acc-1258344699.cos.accelerate.myqcloud.com/"
                     "workbuddy/expert-marketplace/expert_center.json")
# 内置专家团队缺省 id（与清单一致；优先运行时拉清单）
DEFAULT_TEAM_IDS = ["CloudOpsTeam", "SoftwareCompany", "TradingAgentTeam",
                    "GPTResearcherTeam", "MarketingCampaignTeam"]
TEAM_NAMES = {
    "CloudOpsTeam": "腾讯云技术支持", "SoftwareCompany": "软件开发团队",
    "TradingAgentTeam": "交易分析团队", "OpenSpecDocTeam": "专业文档生成团队",
    "GPTResearcherTeam": "深度研究团队", "StockPartnerTeam": "腾讯自选股股票投研专家团",
    "InvestmentMastersTeam": "投资大师专家团", "EngineeringAssuranceTeam": "工程保障团队",
    "HrOperationsTeam": "HR 运营团队", "MarketingCampaignTeam": "营销战役团队",
    "ProductStrategyTeam": "产品战略团队", "SalesBattleTeam": "销售作战团队",
    "ChatLawTeam": "中文法律咨询团", "SoftwareWorkshop": "软件工坊",
    "AShareAnalysis": "A股研究团队",
}


def fetch_team_experts():
    """只读拉一次专家市场清单，返回 {team_id: {categoryId, profession_zh, expertType}}。"""
    out = {}
    try:
        req = urllib.request.Request(EXPERT_MARKET_URL,
                                     headers={"User-Agent": "Mozilla/5.0"})
        with urllib.request.urlopen(req, timeout=20) as r:
            data = json.loads(r.read().decode("utf-8", "replace"))
        for e in (data.get("experts") or []):
            if e.get("expertType") != "team":
                continue
            eid = e.get("id")
            if not eid:
                continue
            prof = e.get("profession") or {}
            out[eid] = {
                "categoryId": e.get("categoryId") or "",
                "profession_zh": prof.get("zh") or "",
                "expertType": "team",
            }
    except Exception as ex:
        print(f"  [warn] 专家清单拉取失败({ex})，回落内置表")
    return out


def team_event(event_code, auth, team_id, team_name="", category="",
               source="builtin", version=""):
    """按客户端源码形状组装单个专家团队事件（/v2/report 信封外字段）。"""
    now = int(time.time() * 1000)
    cid = f"wb-team-{now}"
    rid = f"{cid}-{now}"
    if event_code == "expert_team_summon":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "id": team_id, "name": team_name or team_id,
            "expertTitle": team_name or "", "type": category or "all",
            "userId": auth["uid"],
        }]
    if event_code == "expert_actual_use":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "mode": "CLOUD",
            "id": team_id, "name": team_name or team_id,
            "expertTitle": team_name or "", "type": category or "",
            "expertType": "team", "source": source,
            "version": version or "", "cost": 0, "characterCount": 12,
            "conversationId": cid, "requestId": rid,
            "messageId": rid, "requestModelId": "deepseek-v4-flash",
            "requestModelName": "DeepSeek V4 Flash",
            "userId": auth["uid"],
        }]
    raise ValueError(f"unknown event: {event_code}")


def main():
    ap = argparse.ArgumentParser(description="Expert_team_use_3 任务：N 个不同专家团队事件完成 N/3")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--code", default=DEFAULT_EVENT,
                    choices=["expert_actual_use", "expert_team_summon"])
    ap.add_argument("--count", type=int, default=0,
                    help="本次团队数（默认补到 3）")
    ap.add_argument("--ids", default=None,
                    help="逗号分隔的真实团队 id（默认清单 team 类型前 3 个）")
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

        # 确定团队 id 集合：--ids 优先，否则市场清单 team 列表，否则内置表
        market = fetch_team_experts()
        ids = []
        if a.ids:
            ids = [x.strip() for x in a.ids.split(",") if x.strip()]
        else:
            ordered = list(market.keys()) + \
                [i for i in DEFAULT_TEAM_IDS if i not in market]
            ids = ordered[:need]
        # 去重（同 id 只发一次，3 个不同团队 = 3 个不同 id）
        seen, final = set(), []
        for i in ids:
            if i not in seen:
                seen.add(i)
                final.append(i)
        ids = final[:need]

        if not a.yes:
            print(f"  [dry-run] 将 accept + 上报 {need} 个 {a.code} 事件，"
                  f"团队 id={ids} (当前 {cur}/{target}，加 --yes 生效)")
            for tid in ids:
                meta = market.get(tid) or {}
                payload = team_event(a.code, c, tid,
                                     meta.get("profession_zh") or TEAM_NAMES.get(tid, ""),
                                     meta.get("categoryId", ""))
                print(f"    -> {json.dumps(payload, ensure_ascii=False)}")
            continue

        # 1. accept（任务进度从接单开始计）
        if ast == "not_accepted":
            st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
            print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
            time.sleep(1.05)

        # 2. 依次上报（不同团队 id，模拟 3 个不同专家团）
        for i, tid in enumerate(ids):
            meta = market.get(tid) or {}
            payload = team_event(a.code, c, tid,
                                 meta.get("profession_zh") or TEAM_NAMES.get(tid, ""),
                                 meta.get("categoryId", ""))
            st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
            sc = r_r.get("code") if isinstance(r_r, dict) else r_r
            print(f"  report[{a.code} #{tid}] -> {st_r} code={sc}")
            if i < len(ids) - 1:
                time.sleep(a.gap)

        if a.code == "expert_actual_use":
            time.sleep(2.0)
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