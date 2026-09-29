import sqlite3, json, re

con = sqlite3.connect('file:data/ai_proxy.db?mode=ro', uri=True)
con.text_factory = bytes
cur = con.cursor()

def load(rid, col='req_body'):
    cur.execute('select %s from request_logs where id=?' % col, (rid,))
    row = cur.fetchone()
    return row[0].decode('utf-8', 'replace') if row else None

def try_parse(s):
    try:
        return json.loads(s), None
    except Exception as e:
        return None, str(e)

for rid in [4677, 4678, 4689, 4690]:
    for col in ['req_body_original', 'req_body']:
        s = load(rid, col)
        if s is None:
            print('id', rid, col, 'NULL')
            continue
        obj, err = try_parse(s)
        print('==== id', rid, col, 'chars', len(s), 'bytes', len(s.encode('utf-8')), 'truncated', s.endswith('已截断）'))
        if err:
            print('   parse error:', err)
            continue
        inp = obj.get('input')
        print('   keys:', list(obj.keys()))
        print('   reasoning field:', obj.get('reasoning'))
        if isinstance(inp, list):
            print('   input n=', len(inp))
            for i, it in enumerate(inp):
                if isinstance(it, dict):
                    print('     [%d] type=%r role=%r keys=%s' % (i, it.get('type'), it.get('role'), list(it.keys())))
                else:
                    print('     [%d] non-dict %s' % (i, type(it).__name__))
        elif isinstance(inp, str):
            print('   input is string len', len(inp))
        else:
            print('   input type', type(inp))
con.close()
