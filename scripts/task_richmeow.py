#!/usr/bin/env python3
"""一次性任务脚本：RichMeow_Chat（桌面端对话 1 次，+100 积分 +5 能量 +限定 Buddy 盲盒）.

任务书：先查 eventCode 是否与 chat_request_send 不同（在 probe_active.py / Go 代码
搜 RichMeow 相关——未找到专属 eventCode，REPORT §3.2 也标注「桌面端专属（未破）」）。

实测结论（REPORT §3.2，2026-09-11）：
  - accept 后 3 个账号多次 chat_request_send 上报均不计数（progress 0/1 不动）。
  - 怀疑是桌面端变体的另一套上报通道：queuePendingGrowthTelemetry → 把 growthEvent
    挂在 chat 请求的 extra_vars 里随请求上行（REPORT §1.1），而非 /v2/report。
  - 本脚本默认上线尝试 1 次 chat_request_send（成本可忽略）；若仍 0/1 → 判为不可脚本化。

用法
  python3 task_richmeow.py <uid前缀>             # dry-run
  python3 task_richmeow.py <uid前缀> --yes       # 上报 1 次并回读
"""
import sys, os, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "RichMeow_Chat"
TARGET = 1


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)
    account = sys.argv[1]
    yes = "--yes" in sys.argv

    prefixes = []
    if account.upper() == "ALL":
        import glob
        prefixes = [os.path.basename(p)[10:18]
                    for p in sorted(glob.glob(tc.AUTHS + "/workbuddy-*.json"))]
    else:
        prefixes = [account]

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
        # not_accepted 先 accept
        if ast == "not_accepted" and yes:
            st_a, r_a = tc.accept_tasks(c, [TASK_CODE])
            print(f"  accept -> {st_a} {r_a.get('msg') if isinstance(r_a, dict) else r_a}")
            time.sleep(1.05)
        elif ast == "not_accepted":
            print(f"  [dry-run] 先 accept（当前 not_accepted），再上报 1 条")
        if not yes:
            print(f"  [dry-run] 将上报 1 条 chat_request_send（当前 {cur}/{target}，加 --yes 生效）")
            continue
        results = tc.report_activity(c, count=1, gap=1.05)
        print(f"  report x1 -> {results}")
        time.sleep(1.0)
        st2 = tc.task_status(c, TASK_CODE)
        prog2 = (st2.get("progress") or {}) if st2 else {}
        print(f"  回读 {TASK_CODE}: {prog2.get('current', 0)}/{prog2.get('target', target)} "
              f"accept_status={st2.get('accept_status') if st2 else '?'}")


if __name__ == "__main__":
    main()