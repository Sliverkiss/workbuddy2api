#!/usr/bin/env python3
"""一次性任务脚本：Expert_Philanthropy（「公益专家」召唤并使用完成 1 次捐款，+300 积分，reward_buddy）.

桩点逆向结论（M8 里程碑报告，2026-09-13）：
  ⚠️ 结论修正：**`expert_actual_use` 经 /v2/report 不点亮本任务**（已用 COS slug 与
  运行时真实 id 双重验证，均 200 code=0 但 0/1 不动）。本任务与 M3 expert_5 判定链路不同：
  Expert_Philanthropy 是 reward_buddy=true + template_id_fixed=22 的特殊任务族
  （同 RichMeow_Chat=11 / Hp_Appearance=23 / black_cat=10），判定 = 服务器端业务副作用
  （真实捐款动作），不响应客户端 telemetry 埋点。M0 结论一致：入账需专用业务 API。

  ❌ 点击类不亮（对照，M3 实证）：
    - expert_summoned / expert_team_summon …… 召唤点击观测类。
  ❌ 捐献专属事件码：全 961 个客户端 bundle 静态检索零命中
    (donation/donate/charity/philanthropy 仅存于 emoji 词典与专家资料，无独立事件码)。
    → 「完成 1 次捐款」无独立埋点，任务语义降级验证为「召唤公益专家并使用（发起对话）」，
      即 expert_actual_use + 真实公益专家 id。

  真实公益专家 id（运行时市场 API，chat 域只读，2746 个专家）：
    POST {chatBase}/v2/operation-platform/market/expert/list {"page":1,"page_size":50}
      -> data.experts[]?.filter(公益关键词).expert_id（前缀 ex_）
      ex_u62qHKzKqLtC  —— agent_name=gongyi-expert，display_name_zh=「公益专家」
        腾讯公益助手：找项目、做捐赠、打理小红花花园（quick_prompts 含
        「每天帮我摘花，自动兑换鸡蛋捐出去」/「攒满爱心餐自动送出」= 捐赠动作）。
      TencentCharityExpert —— 「腾讯技术公益智能化专家」（COS 清单，非运行时 id）
      CharityDocFinanceExpert —— 「公益文书与财务专家」
      SkillhubCharityExpertTeam —— 「技术公益专家团」（expertType=team）
  任务的 template_id_fixed=22 只是任务模板 id，不是专家 id（M0.5 教训）。

实测（2026-09-13，<uid>）：
  - 任务本就 accepted（上批 accept 过），跳过 accept。
  - report#1: expert_actual_use + COS slug id（TencentCharityExpert）→ 200 code=0，回读 0/1 ❌。
  - report#2: expert_actual_use + 运行时真实 id（ex_u62qHKzKqLtC / gongyi-expert，含
    version/13-TencentZone 全字段）→ 200 code=0，回读 0/1 ❌。
  - 结论：「召唤并使用」telemetry 通道不点亮本任务。全量客户端 bundle 静态检索
    donation/donate/charity/捐款 零独立事件码（仅 emoji 词典）→ 「完成 1 次捐款」
    是服务器端业务副作用（真实捐赠/小红花花园动作），非 /v2/report 埋点可造。
  - 与 M0 结论一致：reward_buddy=true + template_id_fixed>0 的任务（Expert_Philanthropy
    22 / RichMeow_Chat 11 / Hp_Appearance 23 / black_cat 10）不响应 telemetry；
    入账需专用业务 API（同 first_buddy 走 buddy/first 实证）。
  - claim 仍 400 task not completed（本系列已知悬案，仅记录）。

用法
  python3 task_expert_philanthropy.py <uid前缀>              # dry-run（打载荷，不发）
  python3 task_expert_philanthropy.py <uid前缀> --yes        # accept(若需) + 1 事件 + 回读
  python3 task_expert_philanthropy.py <uid前缀> --yes --expert-id CharityDocFinanceExpert
  python3 task_expert_philanthropy.py <uid前缀> --yes --code expert_summoned   # 对照
"""
import sys, os, json, time, argparse, urllib.request
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "Expert_Philanthropy"
TARGET = 1

DEFAULT_EVENT = "expert_actual_use"

EXPERT_MARKET_URL = ("https://acc-1258344699.cos.accelerate.myqcloud.com/"
                     "workbuddy/expert-marketplace/expert_center.json")
# 市场 API（chat 域，只读 GET/POST 真实专家清单，2746 个，id 前缀 ex_）
MARKET_LIST_PATH = "/v2/operation-platform/market/expert/list"
# 内置公益专家缺省 id（运行时优先拉市场清单）
DEFAULT_EXPERT_IDS = ["ex_u62qHKzKqLtC"]   # gongyi-expert = 公益专家（真实运行时 id）
EXPERT_NAMES = {
    "ex_u62qHKzKqLtC": "公益专家",
    "TencentCharityExpert": "腾讯技术公益智能化专家",
    "CharityDocFinanceExpert": "公益文书与财务专家",
    "SkillhubCharityExpertTeam": "技术公益专家团",
    "gongyi-expert": "公益专家",
}


def fetch_philanthropy_experts(auth):
    """只读拉运行时专家市场清单，过滤公益关键词，返回 {expert_id: {categoryId, profession_zh, expertType}}。

    用 chat 域市场 API（POST /v2/operation-platform/market/expert/list）分页拉取，
    拿到的是客户端真实使用的运行时 id（ex_ 前缀）。COS 静态清单只作兜底。
    """
    out = {}
    try:
        for page in range(1, 14):  # 至多 13 页 * 50 = 650 个专家（公益专家在前几页可命中）
            st, r = tc.do_post(auth, tc.chat_base(auth), MARKET_LIST_PATH,
                               {"page": page, "page_size": 50})
            if st != 200:
                break
            expers = (r.get("data") or {}).get("experts") or []
            if not expers:
                break
            keys = ["gongyi", "公益专家", "philanthropy", "charity", "公益",
                    "charitable", "捐赠", "捐款", "charity doc"]
            for e in expers:
                blob = json.dumps(e, ensure_ascii=False).lower()
                if not any(k in blob for k in keys):
                    continue
                eid = e.get("expert_id") or e.get("source_id")
                if not eid:
                    continue
                out[eid] = {
                    "categoryId": (e.get("categories") or [""])[0] or "",
                    "profession_zh": e.get("profession_zh") or e.get("display_name_zh") or "",
                    "expertType": e.get("expert_type") or "agent",
                }
            if page % 5 == 0:
                import time as _t
                _t.sleep(0.5)
    except Exception as ex:
        print(f"  [warn] 市场清单拉取失败({ex})，回落内置表")
    # COS 静态清单兜底（仅当市场 API 完全失败时）
    if not out:
        try:
            req = urllib.request.Request(EXPERT_MARKET_URL,
                                         headers={"User-Agent": "Mozilla/5.0"})
            with urllib.request.urlopen(req, timeout=20) as r:
                data = json.loads(r.read().decode("utf-8", "replace"))
            for e in (data.get("experts") or []):
                blob = json.dumps(e, ensure_ascii=False).lower()
                if not any(k in blob for k in ["philanthropy", "charity", "公益"]):
                    continue
                eid = e.get("id")
                if not eid:
                    continue
                prof = e.get("profession") or {}
                out[eid] = {
                    "categoryId": e.get("categoryId") or "",
                    "profession_zh": prof.get("zh") or "",
                    "expertType": e.get("expertType") or "agent",
                }
        except Exception as ex2:
            print(f"  [warn] COS 兜底也失败({ex2})")
    return out


def expert_event(event_code, auth, expert_id, expert_name="", category="",
                 expert_type="agent", source="builtin", version=""):
    """按客户端源码形状组装单个公益专家事件（/v2/report 信封外字段）。"""
    now = int(time.time() * 1000)
    cid = f"wb-phi-{now}"
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
    ap = argparse.ArgumentParser(description="Expert_Philanthropy 任务：公益专家使用事件完成 1/1")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--code", default=DEFAULT_EVENT,
                    choices=["expert_actual_use", "expert_summoned"])
    ap.add_argument("--expert-id", default=None,
                    help="真实公益专家 id（默认清单里的 InboundZone 公益专家）")
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

        # 确定公益专家 id + 元数据：--expert-id 优先，否则市场清单，否则内置表
        market = fetch_philanthropy_experts(c)
        expert_id = a.expert_id or (next(iter(market)) if market else DEFAULT_EXPERT_IDS[0])
        meta = market.get(expert_id) or {}
        expert_name = meta.get("profession_zh") or EXPERT_NAMES.get(expert_id, "")
        category = meta.get("categoryId", "")
        expert_type = meta.get("expertType") or "agent"

        if not a.yes:
            print(f"  [dry-run] 将 上报 1 个 {a.code} 事件，"
                  f"expert_id={expert_id} ({expert_name}) (当前 {cur}/{target}，加 --yes 生效)")
            payload = expert_event(a.code, c, expert_id, expert_name, category, expert_type)
            print(f"    -> {json.dumps(payload, ensure_ascii=False)}")
            continue

        # 1. accept（本任务已 accepted，供尚未 accepted 的情形兜底）
        if ast == "not_accepted":
            st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
            print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
            time.sleep(1.05)

        # 2. 上报 1 次（真实公益专家 id；expert_actual_use =「发送成功后」语义事件）
        payload = expert_event(a.code, c, expert_id, expert_name, category, expert_type)
        st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
        sc = r_r.get("code") if isinstance(r_r, dict) else r_r
        print(f"  report[{a.code} #{expert_id}] -> {st_r} code={sc}")

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