"""一次性清理：把「上游整流下发」造成的不可信 TPS 清零。

口径与 internal/proxy/server.go 的 maxPlausibleTPS 一致：流式请求里 tps > 500。
这些行的生成窗口只有几毫秒，算出来的速度是投递假值；清零后界面显示为 —，
也不再进「平均输出速度」统计。

用法：
  python scripts/_clear_implausible_tps.py --dry-run   # 只列清单，不写库
  python scripts/_clear_implausible_tps.py             # 执行清零
"""

import os
import sqlite3
import sys
from datetime import datetime, timedelta

LIMIT = 500
DB = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "data", "ai_proxy.db"
)
dry_run = "--dry-run" in sys.argv


def local(ts):
    """库里存的是 UTC，按本机时区（+8）显示，便于对照日志页。"""
    try:
        return (
            datetime.strptime(ts[:19], "%Y-%m-%dT%H:%M:%S") + timedelta(hours=8)
        ).strftime("%m-%d %H:%M:%S")
    except Exception:
        return ts


con = sqlite3.connect(
    "file:{}?mode={}".format(DB.replace("\\", "/"), "ro" if dry_run else "rw"),
    uri=True,
    timeout=30,
)
con.execute("PRAGMA busy_timeout = 15000")
cur = con.cursor()

rows = cur.execute(
    """
    SELECT id, ts_start, provider_name, model, completion_tokens, ttft_ms, total_ms, tps
    FROM request_logs WHERE stream = 1 AND tps > ? ORDER BY id
    """,
    (LIMIT,),
).fetchall()

print("命中 {} 行（stream=1 且 tps > {}）：".format(len(rows), LIMIT))
for r in rows:
    window = (r[6] or 0) - (r[5] or 0)
    print(
        "  id={} {} {} / {} output={} 窗口={}ms 旧tps={:.1f}".format(
            r[0], local(r[1]), r[2], r[3], r[4], window, r[7] or 0
        )
    )

if dry_run:
    print("（dry-run，未写入）")
else:
    total_before = cur.execute("SELECT count(*) FROM request_logs").fetchone()[0]
    cur.execute("UPDATE request_logs SET tps = 0 WHERE stream = 1 AND tps > ?", (LIMIT,))
    con.commit()
    print("已清零 {} 行".format(cur.rowcount))
    left = cur.execute(
        "SELECT count(*) FROM request_logs WHERE tps > ?", (LIMIT,)
    ).fetchone()[0]
    total_after = cur.execute("SELECT count(*) FROM request_logs").fetchone()[0]
    print("复查：全库 tps > {} 的行数 = {}".format(LIMIT, left))
    print("复查：总行数 {} → {}（不应变化）".format(total_before, total_after))

con.close()
