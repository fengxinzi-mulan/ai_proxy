import sqlite3, json, re

con = sqlite3.connect('file:data/ai_proxy.db?mode=ro', uri=True)
con.text_factory = bytes
cur = con.cursor()

def load(rid):
    cur.execute('select req_body from request_logs where id=?', (rid,))
    return cur.fetchone()[0].decode('utf-8', 'replace')

def extract_input(s):
    k = s.find('"input":')
    if k < 0:
        return None
    i = s.find('[', k)
    depth = 0; instr = False; esc = False
    for j in range(i, len(s)):
        c = s[j]
        if instr:
            if esc: esc = False
            elif c == '\\': esc = True
            elif c == '"': instr = False
        else:
            if c == '"': instr = True
            elif c == '[': depth += 1
            elif c == ']':
                depth -= 1
                if depth == 0:
                    return s[i:j+1]
    return None

def summarize(items, rid):
    print('==== id', rid)
    for idx, it in enumerate(items):
        if not isinstance(it, dict):
            continue
        t = it.get('type'); r = it.get('role')
        if t == 'message' and r == 'assistant':
            c = it.get('content')
            print('   [%d] assistant message content:' % idx, json.dumps(c, ensure_ascii=False)[:300])
            print('        keys:', list(it.keys()))
        elif t == 'function_call':
            print('   [%d] function_call name=%s args=%s' % (idx, it.get('name'), str(it.get('arguments'))[:120]))
        elif t == 'reasoning':
            print('   [%d] reasonING item: %s' % (idx, json.dumps(it, ensure_ascii=False)[:300]))

# Try full parse first (only works when not truncated)
for rid in [4679, 4680]:
    s = load(rid)
    arr = extract_input(s)
    items = json.loads(arr)
    summarize(items, rid)
con.close()
