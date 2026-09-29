import sqlite3, json

con = sqlite3.connect('file:data/ai_proxy.db?mode=ro', uri=True)
con.text_factory = bytes
cur = con.cursor()

def load(rid):
    cur.execute('select req_body from request_logs where id=?', (rid,))
    return cur.fetchone()[0].decode('utf-8', 'replace')

for rid in [4566, 4567, 4514, 4513, 4512, 4511, 4510, 4509]:
    s = load(rid)
    try:
        obj = json.loads(s)
    except Exception as e:
        print('id', rid, 'parse error', e)
        continue
    inp = obj.get('input')
    print('==== id', rid, 'keys', list(obj.keys()))
    print('   reasoning=', obj.get('reasoning'))
    types = []
    if isinstance(inp, list):
        for it in inp:
            if isinstance(it, dict):
                types.append((it.get('type'), it.get('role')))
            else:
                types.append(type(it).__name__)
    print('   input n=%d types=%s' % (len(inp) if isinstance(inp, list) else -1, types))
    # check any reasoning item anywhere
    print('   "type":"reasoning" present:', s.count('"type":"reasoning"'), ' reasoning_content:', s.count('reasoning_content'), ' reasoning_text:', s.count('reasoning_text'))
con.close()
