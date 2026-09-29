import sqlite3, json, re, sys

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
    depth = 0
    instr = False
    esc = False
    for j in range(i, len(s)):
        c = s[j]
        if instr:
            if esc:
                esc = False
            elif c == '\\':
                esc = True
            elif c == '"':
                instr = False
        else:
            if c == '"':
                instr = True
            elif c == '[':
                depth += 1
            elif c == ']':
                depth -= 1
                if depth == 0:
                    return s[i:j+1]
    return None

for rid in [4678, 4679, 4680]:
    s = load(rid)
    arr = extract_input(s)
    print('==== id', rid, 'input extracted:', arr is not None, 'len', len(arr) if arr else 0)
    if arr:
        try:
            items = json.loads(arr)
        except Exception as e:
            print('   parse error', e)
            continue
        print('   n items:', len(items))
        for idx, it in enumerate(items):
            if isinstance(it, dict):
                t = it.get('type'); r = it.get('role'); keys = list(it.keys())
                desc = ''
                if t == 'function_call':
                    desc = ' name=' + str(it.get('name')) + ' call_id=' + str(it.get('call_id'))
                if t == 'function_call_output':
                    desc = ' call_id=' + str(it.get('call_id'))
                if t == 'reasoning':
                    desc = ' ' + json.dumps(it)[:200]
                print('   [%d] type=%r role=%r keys=%s%s' % (idx, t, r, keys, desc))
            else:
                print('   [%d] %s: %s' % (idx, type(it).__name__, str(it)[:60]))
con.close()
