import sqlite3, json, re

con = sqlite3.connect('file:data/ai_proxy.db?mode=ro', uri=True)
con.text_factory = bytes
cur = con.cursor()

# rows that are NOT truncated (body < 64000 chars)
print("=== openai-responses rows with small bodies (likely full) ===")
cur.execute("""select id, ts_start, http_status, length(req_body), length(req_body_original)
               from request_logs where api_format='openai-responses' and length(req_body) < 40000
               order by id desc limit 8""")
for r in cur.fetchall():
    print(r[0], r[1].decode(), 'st', r[2], 'len', r[3], 'orig', r[4])

def load(rid):
    cur.execute('select req_body from request_logs where id=?', (rid,))
    return cur.fetchone()[0].decode('utf-8', 'replace')

# extract top-level scalar keys from a (possibly truncated) body
def top_keys(s):
    keys = []
    for m in re.finditer(r'"([a-zA-Z_]+)":(?:")?', s):
        keys.append(m.group(1))
    return keys

for rid in [4678]:
    s = load(rid)
    print('=== id', rid, 'len', len(s))
    # keys that appear right before tools/after input
    for kw in ['"store"', '"include"', '"previous_response_id"', '"max_output_tokens"',
               '"model"', '"reasoning"', '"stream"', '"tool_choice"', '"instructions"',
               '"parallel_tool_calls"', '"prompt_cache_key"']:
        print('   ', kw, 'count=', s.count(kw))
con.close()
