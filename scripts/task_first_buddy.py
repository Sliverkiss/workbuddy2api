#!/usr/bin/env python3
"""一次性任务脚本：first_buddy（领取一只 Buddy，+300 积分 +8 能量）.

链路（照抄 probe_active.py 的 unlock 模式，实测可行）：
  report(1 条 chat_request_send) -> buddy/agreement -> buddy/first
判据是服务端行为事件，不是 accept 状态。

用法
  python3 task_first_buddy.py <uid前缀>            # dry-run
  python3 task_first_buddy.py <uid前缀> --yes      # 真正执行
  python3 task_first_buddy.py ALL --yes            # 全池（跳过已 claimed）
"""
import sys, os, time
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "first_buddy"


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)
    account = sys.argv[1]
    yes = "--yes" in sys.argv
    gap = 1.05

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
        if ast == "claimed":
            print(f"  [skip] {TASK_CODE} 已 claimed，无需处理")
            continue
        if not yes:
            print(f"  [dry-run] 将执行 report + agreement + buddy/first （加 --yes 生效；当前 accept_status={ast}）")
            continue

        # 写链路
        st_code, code = tc.report_activity(c, count=1, gap=gap)[0]
        print(f"  report      -> {st_code} code={code}")
        time.sleep(2)
        st2, ag = tc.do_post(c, tc.chat_base(c), tc.PATH_BUDDY_AGREEMENT, {"agree": True})
        print(f"  agreement   -> {st2} code={ag.get('code')}")
        st3, bf = tc.do_post(c, tc.chat_base(c), tc.PATH_BUDDY_FIRST, {})
        d = bf.get("data") or {}
        print(f"  buddy/first -> {st3} {bf.get('msg')} credit={d.get('credit')} energy={d.get('energy')}")
        time.sleep(gap)


if __name__ == "__main__":
    main()