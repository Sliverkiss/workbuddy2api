#!/usr/bin/env python3
"""一次性积分任务脚本的共享函数库.

与 probe_active.py 同风格：从 auths/ 读账号凭证，封装 growth 域 / report 域
请求，供 task_*.py 复用。全部默认 dry-run（写动作由调用脚本显式 --yes 放行）。

端点权威来源（Go 代码实测 + 本次实测确认）：
  - chat 域（copilot.tencent.com）：growth / tasks / buddy / streak / chat/completions
  - billing 域（www.codebuddy.cn）：/v2/report
  - accept : POST /v2/activity/growth/tasks/accept  {"task_codes":[code]}
  - claim  : POST /v2/activity/growth/tasks/reward/claim {"task_code":code}
"""
import json, os, time, glob, urllib.request, urllib.error

AUTHS = "/root/workbuddy2api/auths"
CHAT_BASE = "https://copilot.tencent.com"   # growth / tasks / buddy / streak / chat
BILL_BASE = "https://www.codebuddy.cn"      # report / billing

# growth 域常量（travel.go / report.go 与本次实测对齐）
PATH_LIST_TASKS     = "/v2/activity/growth/tasks"
PATH_ACCEPT_TASKS   = "/v2/activity/growth/tasks/accept"
PATH_CLAIM_REWARD   = "/v2/activity/growth/tasks/reward/claim"
PATH_BUDDY_FIRST    = "/activity/growth/buddy/first"
PATH_BUDDY_AGREEMENT = "/activity/growth/buddy/agreement"
PATH_STREAK         = "/activity/growth/streak"
PATH_REPORT         = "/v2/report"
PATH_CHAT           = "/v2/chat/completions"

CLIENT_UA = "CLI/2.63.2 CodeBuddy/2.63.2"


def load_auth(uid_or_file: str) -> dict:
    """从 auths/ 加载账号凭证，uid_or_file 为 uid 前缀或 auths 文件名。

    返回 {token, uid, domain, nick, file} 五元组。
    """
    if os.path.sep in uid_or_file or uid_or_file.endswith(".json"):
        p = uid_or_file
        if not os.path.isabs(p):
            p = os.path.join(AUTHS, p)
    else:
        pre = uid_or_file
        hits = glob.glob(os.path.join(AUTHS, f"workbuddy-{pre}*.json"))
        if not hits:
            raise SystemExit(f"no auth for {pre}")
        p = hits[0]
    d = json.load(open(p))
    a, acc = d["auth"], d["account"]
    return {"token": a["accessToken"], "domain": a.get("domain") or "",
            "uid": acc["uid"], "nick": acc.get("nickname", ""),
            "file": os.path.basename(p)}


def chat_base(auth: dict) -> str:
    return auth.get("chat_base") or CHAT_BASE


def billing_base(auth: dict) -> str:
    return auth.get("billing_base") or BILL_BASE


def _headers(auth: dict) -> dict:
    hdr = {"Authorization": "Bearer " + auth["token"],
           "Accept": "application/json",
           "Content-Type": "application/json",
           "User-Agent": CLIENT_UA,
           "Origin": "https://www.codebuddy.cn",
           "Referer": "https://www.codebuddy.cn/"}
    if auth.get("uid"):
        hdr["X-User-Id"] = auth["uid"]
    if auth.get("domain"):
        hdr["X-Domain"] = auth["domain"]
    return hdr


def _request(auth, method, base, path, body=None, headers=None, timeout=30):
    url = path if path.startswith("http") else base + path
    hdr = _headers(auth)
    if headers:
        hdr.update(headers)
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, headers=hdr, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        t = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(t)
        except Exception:
            return e.code, {"raw": t[:300]}
    except Exception as e:
        return -1, {"err": repr(e)}


def do_get(auth, base, path, headers=None) -> tuple:
    """GET 读请求，返回 (status, dict)。"""
    return _request(auth, "GET", base, path, None, headers)


def do_post(auth, base, path, body, headers=None) -> tuple:
    """POST 写请求，返回 (status, dict)。body 为 dict。"""
    return _request(auth, "POST", base, path, body, headers)


def list_tasks(auth) -> list:
    """GET /v2/activity/growth/tasks 全量任务列表（元素为原始 dict）。"""
    st, d = do_get(auth, chat_base(auth), PATH_LIST_TASKS)
    if st != 200:
        raise RuntimeError(f"list_tasks http={st}")
    tasks = (d.get("data", {}) or {}).get("tasks") or []
    return tasks


def task_status(auth, task_code) -> dict | None:
    """查单个任务当前状态；找不到返回 None。"""
    for t in list_tasks(auth):
        if t.get("task_code") == task_code:
            return t
    return None


def accept_tasks(auth, task_codes) -> tuple:
    """POST accept 任务（not_accepted → accepted）。返回 (status, resp)。"""
    return do_post(auth, chat_base(auth), PATH_ACCEPT_TASKS,
                   {"task_codes": task_codes})


def claim_reward(auth, task_code) -> tuple:
    """POST claim 领取奖励（任务已 complete 后可领）。重复领返回业务错误，安全。"""
    return do_post(auth, chat_base(auth), PATH_CLAIM_REWARD,
                   {"task_code": task_code})


def get_streak(auth) -> int:
    """GET /activity/growth/streak 连登天数（只读 oracle）。失败返回 -1 记日志。"""
    st, d = do_get(auth, chat_base(auth), PATH_STREAK)
    if st != 200:
        return -1
    return (d.get("data", {}).get("streak", {}) or {}).get("days", 0)


def chat_event(auth, conversation_id=None, model_id="deepseek-v4-flash",
               model_name="DeepSeek V4 Flash", mode="craft"):
    """客户端 chat_request_send 事件完整形状（照抄 report.go / probe_active.py）。

    必须带 userId（=账号 uid），缺失则服务端 200 但静默丢弃。
    model_id/name 可换（如 GLM-5.2），供 model_chat 对齐实际模型。
    """
    now = int(time.time() * 1000)
    cid = conversation_id or f"task-{now}"
    return {"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
            "mode": mode, "conversationId": cid, "requestId": cid,
            "inputLength": 12, "requestModelId": model_id,
            "requestModelName": model_name, "isPlan": False,
            "isAutoExecuteTerminal": False, "isAutoModify": False,
            "codebaseEnable": False, "maxToken": 0, "maxSteps": 0, "temperature": 0,
            "maxRetries": 0, "mentionContexts": [], "knowledgeId": [],
            "knowledgeName": [], "codebaseId": "", "mentionContextCount": 0,
            "command": "", "expertId": "", "recommendId": "", "skillId": "",
            "skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
            "traceId": "", "rootRequestId": cid, "parentConversationId": cid,
            "agentName": "default", "agentType": "conversation", "userId": auth["uid"]}


def report_activity(auth, count=1, gap=1.05, model_id="deepseek-v4-flash",
                    model_name="DeepSeek V4 Flash", mode="craft") -> list:
    """向 {billing}/v2/report 上报 count 条 chat_request_send。

    每次间隔 >= gap 秒（默认 1.05，匹配 probe_active.py 的同接口限速口径）。
    返回 [(status, code), ...] 汇总。
    """
    out = []
    for i in range(count):
        ev = chat_event(auth, model_id=model_id, model_name=model_name, mode=mode)
        st, r = do_post(auth, billing_base(auth), PATH_REPORT, [ev])
        out.append((st, r.get("code") if isinstance(r, dict) else None))
        if i < count - 1:
            time.sleep(gap)
    return out


def chat_completion(auth, model_id="glm-5.2", prompt="hi", max_tokens=32,
                    timeout=60, extra_var=None) -> tuple:
    """POST {chat}/v2/chat/completions 真实对话一次（stream:true）。

    服务端强制流式（payload.go 同款口径），这里逐行读 SSE 直到 done。
    返回 (status, first_content)。用于 Model_chat_GLM5.2 的“真实对话一次”。

    extra_var（可选）：顶层 extra_vars 覆盖/新增字段（如 growthEvent），
    模拟桌面端 requestOptions.providerData 的透传形状。
    """
    body = {"model": model_id, "messages": [{"role": "user", "content": prompt}],
            "stream": True, "max_tokens": max_tokens}
    if extra_var:
        body["extra_vars"] = {**(body.get("extra_vars") or {}), **extra_var}
    hdr = {"Accept": "text/event-stream"}  # SSE
    url = chat_base(auth) + PATH_CHAT
    req_headers = _headers(auth)
    req_headers.update(hdr)
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers=req_headers, method="POST")
    first = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            status = r.status
            for raw in r:
                line = raw.decode("utf-8", "replace")
                if line.startswith("data: "):
                    payload = line[6:].strip()
                    if payload in ("[DONE]", ""):
                        continue
                    try:
                        obj = json.loads(payload)
                        delta = (obj.get("choices") or [{}])[0].get("delta") or {}
                        content = delta.get("content") or ""
                        if content and not first:
                            first = content
                    except Exception:
                        pass
            return status, first
    except urllib.error.HTTPError as e:
        t = e.read().decode("utf-8", "replace")
        return e.code, t[:200]
    except Exception as e:
        return -1, repr(e)[:200]