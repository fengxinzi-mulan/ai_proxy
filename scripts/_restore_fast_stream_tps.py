"""一次性回填：恢复被旧判据误清零的真实高速流 TPS。

背景：computeTPS 曾经只用「速度 > 500」一条判据拦整流下发，结果把真实的高速
持续流也判成了不可测。2026-11-15 的库里 21980 个 token 在 32 秒内持续到达
（684 t/s）、18845 个 token 用了 35.8 秒（526 t/s），这些都被写成了 0，
界面上显示「—」，最近 200 条里 166 条如此。

新判据（internal/proxy/server.go）要求两个条件同时成立才算整流下发：
速度快（> maxPlausibleTPS）**且** 每 token 耗时短到不可能是逐 token 生成
（< minPlausibleMsPerToken）。本脚本按同一口径把误清零的行算回去。

口径与 Go 侧保持一致，改动阈值时两边要一起改。

用法：
  python scripts/_restore_fast_stream_tps.py --dry-run   # 只列清单，不写库
  python scripts/_restore_fast_stream_tps.py             # 执行回填
"""

import os
import sqlite3
import sys
from datetime import datetime, timedelta

# 与 internal/proxy/server.go 的常量保持一致。
MAX_PLAUSIBLE_TPS = 500
MIN_PLAUSIBLE_MS_PER_TOKEN = 0.5
MIN_TPS_MEASURE_TOKENS = 20

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


def restored_tps(tokens, ttft_ms, total_ms):
    """按新判据算回 TPS；与 Go 的 computeTPS 逐条对应，返回 0 表示仍不可测。"""
    if not tokens or tokens <= 0 or not total_ms or total_ms <= 0:
        return 0.0
    # 输出太少时窗口由网络抖动主导，速度不具参考价值。
    if tokens < MIN_TPS_MEASURE_TOKENS:
        return 0.0
    if ttft_ms and ttft_ms > 0:
        window = total_ms - ttft_ms
        if window <= 0:
            return 0.0
    else:
        window = total_ms
    tps = tokens / (window / 1000.0)
    # 两个判据同时成立才是整流下发：速度快，且每 token 耗时短到不可能逐 token 生成。
    if tps > MAX_PLAUSIBLE_TPS and (window / float(tokens)) < MIN_PLAUSIBLE_MS_PER_TOKEN:
        return 0.0
    return tps


con = sqlite3.connect(
    "file:{}?mode={}".format(DB.replace("\\", "/"), "ro" if dry_run else "rw"),
    uri=True,
    timeout=30,
)
con.execute("PRAGMA busy_timeout = 15000")
cur = con.cursor()

rows = cur.execute(
    """
    SELECT id, ts_start, provider_name, model, completion_tokens, ttft_ms, total_ms
    FROM request_logs
    WHERE stream = 1 AND tps = 0 AND total_ms > ttft_ms AND completion_tokens > 0
    ORDER BY id
    """
).fetchall()

todo = []
for r in rows:
    tps = restored_tps(r[4], r[5], r[6])
    if tps > 0:
        todo.append((r, tps))

print("待回填 {} 行（stream=1、tps=0，按新判据可算出速度）：".format(len(todo)))
for r, tps in todo:
    window = (r[6] or 0) - (r[5] or 0)
    print(
        "  id={} {} {} / {} output={} 窗口={}ms → {:.1f} t/s".format(
            r[0], local(r[1]), r[2], r[3], r[4], window, tps
        )
    )

if dry_run:
    print("（dry-run，未写入）")
else:
    total_before = cur.execute("SELECT count(*) FROM request_logs").fetchone()[0]
    cur.executemany(
        "UPDATE request_logs SET tps = ? WHERE id = ?",
        [(tps, r[0]) for r, tps in todo],
    )
    con.commit()
    print("已回填 {} 行".format(len(todo)))
    total_after = cur.execute("SELECT count(*) FROM request_logs").fetchone()[0]
    print("复查：总行数 {} → {}（不应变化）".format(total_before, total_after))
    left = cur.execute(
        "SELECT count(*) FROM request_logs WHERE stream = 1 AND tps = 0 "
        "AND total_ms > ttft_ms AND completion_tokens > 0"
    ).fetchone()[0]
    print("复查：仍为 0 的流式行 = {}（应只剩真整流下发与窗口退化）".format(left))

con.close()
