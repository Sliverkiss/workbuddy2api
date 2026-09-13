#!/usr/bin/env python3
"""一次性任务脚本：Hp_Appearance（「体验『和平精英』主题」，+100 积分，reward_buddy）.

桩点逆向结论（M12 里程碑，2026-09-13）：
  - 客户端把「使用主题」当 telemetry 上报：EVENT_NAME_MAPPING
    [AgentTelemetryEvents.AppearanceSkinApply] -> "appearance_skin_apply"，
    payload 由 buildSkinApplyPayload 组装：{action:"apply", source:"settings_close",
    id: theme.resourceKey, vipLevel, series, type: editionType}。
    注释明确「皮肤生效：正式使用（离开外观页时复核仍有权限）或档位不足被回退」。
  - **主题目录 API（只读）**：POST {billingBase}/v2/operation-platform/appearance/resources
    {platform:"client", kind:"theme", version, lang} -> data.resources[]。
    实测命中 **theme-tkmw7j = 「和平精英激战金秋」**（vip_level=free, series=craft,
    appearance=light）——即任务所指「和平精英」主题（2026-09-07~11-02 限时）。
  - **真实业务副作用 API**：桌面端 DesktopAppearanceRepo（routePrefix=/v2）POST
    {endpoint}/v2/user-asset/appearance/set {kind:"theme", resource_key:"theme-tkmw7j"}
    —— 这是「点击-外观-使用主题」真正落服务端的动作，语义同 first_buddy 走
    buddy/first 专用业务 API。reward_buddy=true 族判据在业务副作用，故 set 是权威候选。
  - 本族（template_id_fixed>0 + reward_buddy=true，同 Expert_Philanthropy=22 /
    RichMeow_Chat=11 / black_cat=10）M0/M8 已定案：telemetry 通道不可点亮，
    需服务端业务动作或专用业务 API。

  `appearance/set` 切主题后探针号外观会变更到和平精英主题——可回切
  `resource_key="light"` 复原（脚本内置 verify/restore 选项）。

用法
  python3 task_hp_appearance.py <uid前缀>                 # dry-run（打载荷，不发）
  python3 task_hp_appearance.py <uid前缀> --yes           # accept + appearance_skin_apply 上报 + 回读
  python3 task_hp_appearance.py <uid前缀> --yes --code appearance_skin_apply
  python3 task_hp_appearance.py <uid前缀> --yes --code appearance_set   # 真实业务 API 应用主题
  python3 task_hp_appearance.py <uid前缀> --yes --restore light         # 回切主题（收尾复原）
"""
import sys, os, json, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "Hp_Appearance"
TARGET = 1

DEFAULT_THEME_ID = "theme-tkmw7j"
DEFAULT_THEME_NAME = "和平精英激战金秋"
PATH_APPEARANCE_CATALOG = "/v2/operation-platform/appearance/resources"
PATH_APPEARANCE_SET = "/v2/user-asset/appearance/set"

THEME_NAMES = {DEFAULT_THEME_ID: DEFAULT_THEME_NAME}


def fetch_pe_theme(auth):
    """只读拉主题目录，过滤「和平精英」，返回 {resource_id: {name, vipLevel, series, appearance}}。"""
    out = {}
    try:
        st, r = tc.do_post(auth, tc.billing_base(auth), PATH_APPEARANCE_CATALOG,
                           {"platform": "client", "kind": "theme",
                            "version": tc.CLIENT_UA.split("/")[-1], "lang": "zh-CN"})
        res = ((r.get("data") or {}).get("resources") or []) if isinstance(r, dict) else []
        for x in res:
            name = x.get("name") or ""
            if "和平精英" in name or "pubg" in name.lower():
                out[x.get("id")] = {
                    "name": name, "vipLevel": x.get("vip_level", ""),
                    "series": x.get("series", ""), "appearance": x.get("appearance", ""),
                }
    except Exception as ex:
        print(f"  [warn] 主题目录拉取失败({ex})，回落内置表")
    if not out:
        out = {DEFAULT_THEME_ID: {"name": DEFAULT_THEME_NAME, "vipLevel": "free",
                                  "series": "craft", "appearance": "light"}}
    return out


def skin_apply_event(auth, theme_id, theme_name="", vip_level="free", series="craft",
                     edition_type="unknown", source="settings_close"):
    """按客户端 buildSkinApplyPayload 形状组装 appearance_skin_apply 事件。"""
    now = int(time.time() * 1000)
    return [{
        "eventCode": "appearance_skin_apply", "timestamp": now, "reportDelay": 0,
        "action": "apply", "source": source,
        "id": theme_id, "vipLevel": vip_level, "series": series,
        "type": edition_type, "name": theme_name or theme_id,
        "userId": auth["uid"],
    }]


def set_theme(auth, resource_key):
    """真实业务动作：POST /v2/user-asset/appearance/set（桌面端 DesktopAppearanceRepo 同款）。"""
    return tc.do_post(auth, tc.billing_base(auth), PATH_APPEARANCE_SET,
                      {"kind": "theme", "resource_key": resource_key})


def main():
    ap = argparse.ArgumentParser(description="Hp_Appearance 任务：和平精英主题使用完成 1/1")
    ap.add_argument("account", help="uid 前缀")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--code", default="appearance_skin_apply",
                    choices=["appearance_skin_apply", "appearance_set"])
    ap.add_argument("--theme-id", default=None, help="真实主题 resourceKey（默认 theme-tkmw7j≈和平精英激战金秋）")
    ap.add_argument("--restore", default=None, choices=["light", "dark"],
                    help="收尾回切主题（如切过 set 后复原），不需 --yes")
    a = ap.parse_args()

    c = tc.load_auth(a.account)
    print(f"== {c['uid'][:8]} ({c['nick']}) ==")

    # 收尾复原分支
    if a.restore:
        st_s, r_s = set_theme(c, a.restore)
        msg = r_s.get("msg") if isinstance(r_s, dict) else r_s
        print(f"  restore appearance/set {a.restore} -> {st_s} {msg}")
        return

    try:
        st = tc.task_status(c, TASK_CODE)
    except Exception as e:
        print(f"  [skip] list_tasks 失败: {e}")
        sys.exit(1)
    if st is None:
        print(f"  [skip] 无 {TASK_CODE} 任务")
        sys.exit(0)
    ast = st.get("accept_status")
    prog = (st.get("progress") or {})
    cur = prog.get("current", 0)
    target = prog.get("target", TARGET)
    if ast == "claimed" or cur >= target:
        print(f"  [skip] 已完成 {cur}/{target} (accept_status={ast})")
        sys.exit(0)

    themes = fetch_pe_theme(c)
    theme_id = a.theme_id or (next(iter(themes)) if themes else DEFAULT_THEME_ID)
    meta = themes.get(theme_id) or THEME_NAMES.get(theme_id, {})
    theme_name = meta.get("name") or theme_id
    vip = meta.get("vipLevel", "free")
    series = meta.get("series", "craft")

    if not a.yes:
        print(f"  [dry-run] accept(若需) + {a.code} 主题={theme_id} ({theme_name})"
              f" (当前 {cur}/{target}，加 --yes 生效)")
        if a.code == "appearance_skin_apply":
            print(f"    -> {json.dumps(skin_apply_event(c, theme_id, theme_name, vip, series), ensure_ascii=False)}")
        else:
            print(f"    -> POST {PATH_APPEARANCE_SET} {json.dumps({'kind':'theme','resource_key':theme_id})}")
        sys.exit(0)

    if ast == "not_accepted":
        st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
        print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
        time.sleep(1.05)

    if a.code == "appearance_skin_apply":
        payload = skin_apply_event(c, theme_id, theme_name, vip, series)
        st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
        sc = r_r.get("code") if isinstance(r_r, dict) else r_r
        print(f"  report[appearance_skin_apply #{theme_id}] -> {st_r} code={sc}")
    else:
        st_r, r_r = set_theme(c, theme_id)
        sc = r_r.get("code") if isinstance(r_r, dict) else r_r
        msg = r_r.get("msg") if isinstance(r_r, dict) else ""
        print(f"  appearance/set #{theme_id} -> {st_r} code={sc} {msg}")
        print(f"  （已切到和平精英主题；如需复原：脚本 --restore light）")

    time.sleep(2.0)
    st2 = tc.task_status(c, TASK_CODE)
    prog2 = (st2.get("progress") or {}) if st2 else {}
    cur2 = prog2.get("current", 0)
    ast2 = st2.get("accept_status") if st2 else "?"
    print(f"  回读 {TASK_CODE}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")


if __name__ == "__main__":
    main()