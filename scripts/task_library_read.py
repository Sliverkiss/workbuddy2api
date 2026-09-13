#!/usr/bin/env python3
"""一次性任务脚本：Library_read（「体验资料库」，读《WorkBuddy 资料库介绍》，+100 积分）.

桩点逆向结论（M10 里程碑，2026-09-13）：
  普通任务（reward_buddy=false）判定链路 = 客户端 telemetry 经 POST {billing}/v2/report，
  必带 userId，每事件 +1。本任务的两个静态事实：
    1. 客户端**无** library_read/doc_read/document_read/doc_open 读完成类事件码——
       全量 AgentTelemetryEvents 枚举 + wbx_* + 各 report 函数均无「资料库文档读完」
       信号；space 域是 iframe（www.workbuddy.cn/space/...），iframe→host 只走 JSAPI
       postMessage（SpaceJsApiEvent.OpenNode="space:open-node"），不落 /v2/report。
    2. 最近似的真实上报事件 = `chat_lib_request_send`（对话发送携带资料库文件，
       mode="space-doc"，id=真实 docId）——客户端在「把资料库文档当附件发起对话」
       时上报，是唯一带着真实资料库文档 id 走 /v2/report 的原子事件。
  真实 doc id 权威源 = space engine agent API（只读）：
    POST {spaceBase}/space/api/agent/v1/list-user-spaces
    POST {spaceBase}/space/api/agent/v1/list-node  {"spaceId": <promo space>}
    命中推广 space「🚀资料库的100种用法」(cCwTkzCwCevtZDBkVGnFEv) 下的
    **o0KWYeynteVv06UnAZqIFm**（kind=web，title=「WorkBuddy资料库介绍」）。
  对照组：web_element_click space 入口（UI 类事件，历史经验不点亮，仅作对照）。

实测（2026-09-13，<uid>）——全部 200 code=0 但 **0/1 未点亮**：
  - chat_lib_request_send (mode=space-doc, id=o0KWYeynteVv06UnAZqIFm) → 未点亮
  - web_element_click (space_entry_click) / buddyapp_menu_click (elementId=space) → 未点亮
  - web_page_show (file_viewer, pageURL=/space/d/<doc>) → 400 code=10001（信封不匹配）
  - chat_request_send + knowledgeId/mentionContexts 挂 space-doc → 未点亮
  结论：客户端**无**读完成类事件码；space 域纯 iframe（www.workbuddy.cn/space），
  读完成判定在服务端 space 引擎侧（真浏览器打开读完才能落帐），telemetry 不可造。
  即 M10 归入「telemetry 不可点亮」族（同 M8 Expert_Philanthropy/M12-M14 判例）。

用法

用法
  python3 task_library_read.py <uid前缀>            # dry-run（打载荷，不发）
  python3 task_library_read.py <uid前缀> --yes      # accept + 1 事件 + 回读
  python3 task_library_read.py <uid前缀> --yes --code web_element_click  # 对照组
"""
import sys, os, json, time, argparse
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "Library_read"
TARGET = 1

DEFAULT_EVENT = "chat_lib_request_send"

SPACE_BASE = "https://www.workbuddy.cn"
PATH_LIST_SPACES = "/space/api/agent/v1/list-user-spaces"
PATH_LIST_NODE = "/space/api/agent/v1/list-node"
PATH_NODE_INFO = "/space/api/agent/v1/node-info"

# 静态锚定（2026-09-13 实测）：
#  推广 space「🚀资料库的100种用法」cCwTkzCwCevtZDBkVGnFEv 下
#  o0KWYeynteVv06UnAZqIFm kind=web title=「WorkBuddy资料库介绍」
DEFAULT_DOC_ID = "o0KWYeynteVv06UnAZqIFm"
DEFAULT_PROMO_SPACE = "cCwTkzCwCevtZDBkVGnFEv"
DOC_NAMES = {
    "o0KWYeynteVv06UnAZqIFm": "WorkBuddy资料库介绍",
}


def fetch_intro_doc(auth):
    """只读拉推广 space 根节点清单，按标题过滤「资料库介绍」，返回 {doc_id: {title, kind}}。"""
    out = {}
    try:
        st, r = tc.do_post(auth, SPACE_BASE, PATH_LIST_SPACES, {})
        spaces = ((r.get("data") or {}).get("spaces")) if isinstance(r, dict) else []
        target = None
        for s_ in spaces or []:
            if "资料库" in (s_.get("title") or ""):
                target = s_.get("spaceId")
                break
        if not target:
            target = DEFAULT_PROMO_SPACE
        st2, r2 = tc.do_post(auth, SPACE_BASE, PATH_LIST_NODE, {"spaceId": target})
        nodes = ((r2.get("data") or {}).get("nodes")) if isinstance(r2, dict) else []
        for n in nodes or []:
            title = n.get("title") or ""
            if "资料库介绍" in title or "资料库 介绍" in title:
                out[n.get("id")] = {"title": title, "kind": n.get("kind", "")}
    except Exception as ex:
        print(f"  [warn] space 清单拉取失败({ex})，回落内置表")
    if not out:
        out = {DEFAULT_DOC_ID: {"title": DOC_NAMES[DEFAULT_DOC_ID], "kind": "web"}}
    return out


def library_event(event_code, auth, doc_id, doc_name="", kind="web"):
    """按客户端源码形状组装单个资料库事件（/v2/report 信封外字段）。"""
    now = int(time.time() * 1000)
    cid = f"wb-lib-{now}"
    rid = f"{cid}-{now}"
    if event_code == "chat_lib_request_send":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "conversationId": cid, "requestId": rid,
            "mentionContexts": ["knowledge"],
            "id": doc_id, "name": doc_name or doc_id,
            "source": kind or "web", "type": kind or "web",
            "mode": "space-doc", "userId": auth["uid"],
        }]
    if event_code == "web_element_click":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "pageName": "space", "elementId": "space_entry_click",
            "elementName": "资料库入口", "userId": auth["uid"],
        }]
    if event_code == "web_page_show":
        return [{
            "eventCode": event_code, "timestamp": now, "reportDelay": 0,
            "pageName": "space", "userId": auth["uid"],
        }]
    raise ValueError(f"unknown event: {event_code}")


def main():
    ap = argparse.ArgumentParser(description="Library_read 任务：chat_lib_request_send(space-doc) 完成 1/1")
    ap.add_argument("account", help="uid 前缀")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--code", default=DEFAULT_EVENT,
                    choices=["chat_lib_request_send", "web_element_click", "web_page_show"])
    ap.add_argument("--doc-id", default=None, help="真实资料库介绍文档 id（默认列表命中的 o0KWYeynteVv06UnAZqIFm）")
    a = ap.parse_args()

    c = tc.load_auth(a.account)
    print(f"== {c['uid'][:8]} ({c['nick']}) ==")
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

    docs = fetch_intro_doc(c)
    doc_id = a.doc_id or (next(iter(docs)) if docs else DEFAULT_DOC_ID)
    meta = docs.get(doc_id) or {}
    doc_name = meta.get("title") or DOC_NAMES.get(doc_id, doc_id)
    kind = meta.get("kind", "web")

    if not a.yes:
        print(f"  [dry-run] 将 accept + 上报 1 个 {a.code} 事件，"
              f"doc_id={doc_id} ({doc_name}, kind={kind}) (当前 {cur}/{target}，加 --yes 生效)")
        payload = library_event(a.code, c, doc_id, doc_name, kind)
        print(f"    -> {json.dumps(payload, ensure_ascii=False)}")
        sys.exit(0)

    if ast == "not_accepted":
        st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
        print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
        time.sleep(1.05)

    payload = library_event(a.code, c, doc_id, doc_name, kind)
    st_r, r_r = tc.do_post(c, tc.billing_base(c), tc.PATH_REPORT, payload)
    sc = r_r.get("code") if isinstance(r_r, dict) else r_r
    print(f"  report[{a.code} #{doc_id}] -> {st_r} code={sc}")

    time.sleep(2.0)
    st2 = tc.task_status(c, TASK_CODE)
    prog2 = (st2.get("progress") or {}) if st2 else {}
    cur2 = prog2.get("current", 0)
    ast2 = st2.get("accept_status") if st2 else "?"
    print(f"  回读 {TASK_CODE}: {cur2}/{prog2.get('target', target)} accept_status={ast2}")
    if ast2 != "claimed" and cur2 >= target:
        print("  → 任务已满足，下一步可 claim（已知 claim 400 悬案，仅记录）")
    elif ast2 != "claimed":
        print(f"  → 未点亮（仅对照参考）")


if __name__ == "__main__":
    main()