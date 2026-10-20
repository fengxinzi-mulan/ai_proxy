"""缓存命中异常诊断：找出「同会话内个别请求缓存率骤降」的规律。

只读打开 data/ai_proxy.db，按响应里的 system_fingerprint 分组统计命中率，
并检查低命中请求与其前后相邻请求的报文前缀是否逐字节一致。

用法：python scripts/cache_diag.py
"""

import collections
import re
import sqlite3

DB = 'file:data/ai_proxy.db?mode=ro'


def load():
    con = sqlite3.connect(DB, uri=True)
    con.text_factory = bytes
    cur = con.cursor()
    cur.execute("""select id,ts_start,prompt_tokens,cached_tokens,req_headers,req_body,resp_body
        from request_logs where path like '%/chat/completions' and prompt_tokens > 0 order by id""")
    out = []
    for rid, ts, pt, ct, hd, body, resp in cur.fetchall():
        dec = lambda v: v.decode('utf-8', 'replace') if isinstance(v, bytes) else v
        fp = set(x.decode() for x in re.findall(rb'"system_fingerprint":"([^"]*)"', resp or b''))
        out.append({
            'id': rid, 'ts': dec(ts), 'pt': pt, 'ct': ct,
            'hit': ct / pt * 100 if pt else 0.0,
            'body': body or b'', 'orig': b'',
            'fp': '/'.join(sorted(fp)),
            'total_len': content_length(hd),
        })
    return out


def content_length(headers):
    txt = headers.decode('utf-8', 'replace') if isinstance(headers, bytes) else (headers or '')
    for line in txt.splitlines():
        if line.lower().startswith('content-length:'):
            return int(line.split(':', 1)[1])
    return 0


def common_prefix(a, b):
    n = min(len(a), len(b))
    for i in range(n):
        if a[i] != b[i]:
            return i
    return n


def main():
    rows = load()
    by_id = {r['id']: r for r in rows}

    print('=== 按上游后端（响应里的 system_fingerprint）分组 ===')
    groups = collections.defaultdict(list)
    for r in rows:
        groups[r['fp'] or '(空)'].append(r)
    for fp, items in sorted(groups.items(), key=lambda kv: -len(kv[1])):
        hits = sorted(x['hit'] for x in items)
        print('%-36s 条数=%-4d 平均命中=%5.1f%% 中位=%5.1f%%' % (
            fp, len(items), sum(hits) / len(hits), hits[len(hits) // 2]))
        low = [x['id'] for x in items if x['hit'] < 50]
        if low:
            print('    命中<50%% 的请求: %s' % low)

    print()
    print('=== 低命中请求（pt>2000 且 ct<=200）===')
    low = [r for r in rows if r['pt'] > 2000 and r['ct'] <= 200]
    for r in low:
        print('#%-6d %s pt=%-7d ct=%-6d 后端=%s' % (r['id'], r['ts'][11:19], r['pt'], r['ct'], r['fp'] or '(空)'))

    print()
    print('=== 低命中请求 vs 相邻同请求：报文前缀是否逐字节一致 ===')
    print('（前缀一致 ⇒ 本地发出的字节流相同，命中率差异来自上游）')
    for r in low:
        prev = max((x for x in rows if x['id'] < r['id']), key=lambda x: x['id'], default=None)
        nxt = min((x for x in rows if x['id'] > r['id']), key=lambda x: x['id'], default=None)
        for other, label in ((prev, '前一条'), (nxt, '后一条')):
            if not other:
                continue
            n = common_prefix(r['body'], other['body'])
            cap = min(len(r['body']), len(other['body']))
            tag = '前缀全同（已达落库截断上限）' if n >= cap else '第 %d 字节起不同' % n
            print('#%d(ct=%d) vs #%d(ct=%d) %s: 公共前缀=%d/%d %s' % (
                r['id'], r['ct'], other['id'], other['ct'], label, n, cap, tag))
        print()


if __name__ == '__main__':
    main()
