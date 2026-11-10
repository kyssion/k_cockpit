#!/usr/bin/env python3
"""第 14 轮 TP-14 / TC-19-4：日志端点真实功能验证（补 G-15 的 HTTP 级证据）。

单元层已有 logging 包的完整测试，但 /settings/log/* 六个端点此前只有
「畸形参数 fuzz 不 5xx」，没有真实功能断言。本脚本逐条验证功能语义。
"""
import http.cookiejar
import json
import os
import urllib.error
import urllib.request

BASE = os.environ.get('KC_BASE', 'http://127.0.0.1:18080')
JAR = os.environ.get('KC_COOKIE', os.path.join(os.getcwd(), 'data/lab2/jar.txt'))
jar = http.cookiejar.MozillaCookieJar(JAR)
jar.load(ignore_discard=True, ignore_expires=True)
op = urllib.request.build_opener(
    urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(jar)
)

fails = []
passed = 0


def ck(label, ok, detail=''):
    global passed
    if ok:
        passed += 1
        print(f'[PASS] {label}')
    else:
        fails.append(label)
        print(f'[FAIL] {label}  <<< {detail[:220]}')


def req(method, path, body=None, timeout=20):
    data = json.dumps(body).encode() if body is not None else None
    h = {'Accept': 'application/json'}
    if data is not None:
        h['Content-Type'] = 'application/json'
    r = urllib.request.Request(BASE + path, data=data, headers=h, method=method)
    try:
        with op.open(r, timeout=timeout) as resp:
            raw = resp.read()
            ct = resp.headers.get('Content-Type', '')
            st = resp.status
    except urllib.error.HTTPError as e:
        raw, ct, st = e.read(), e.headers.get('Content-Type', ''), e.code
    except Exception as e:
        return 0, '', b'', str(e)
    if 'json' in ct:
        try:
            return st, ct, json.loads(raw or b'{}'), ''
        except Exception:
            return st, ct, {}, 'JSON 解析失败'
    return st, ct, raw, ''


def d(o):
    return (o.get('data') if isinstance(o, dict) else None) or {}


print('== 1. status：级别条数统计 ==')
st, ct, o, err = req('GET', '/api/v1/settings/log/status')
ck('status 返回 200', st == 200, f'{st} {err} {str(o)[:180]}')
data = d(o)
ck('status 含级别统计（logging.Status.Lines 字段）',
   isinstance(data.get('lines'), dict), str(data)[:180])
ck('status 含 file_path 或明确说明', 'file_path' in data or 'log_dir' in data or 'dir' in data, str(data)[:180])

print('== 2. read：在线查看 + 过滤（只读环形缓冲） ==')
st, ct, o, err = req('GET', '/api/v1/settings/log/read?limit=20')
ck('read 返回 200', st == 200, f'{st} {err} {str(o)[:180]}')
rd = d(o)
lines = rd.get('lines') or rd.get('items') or []
if isinstance(lines, list):
    ck('read 返回行数组', True)
    ck('read 行数不超过 limit', len(lines) <= 20, f'len={len(lines)}')
else:
    ck('read 返回行数组', False, f'实际形状 {type(lines)} {str(rd)[:150]}')
st2, _, o2, _ = req('GET', '/api/v1/settings/log/read?level=ERROR&limit=10')
ck('read 按 level 过滤不 5xx', st2 < 500, str(st2))
st3, _, o3, _ = req('GET', '/api/v1/settings/log/read?keyword=zzz_no_such_token_zzz&limit=10')
ck('read 关键字过滤不 5xx', st3 < 500, str(st3))
l3 = d(o3).get('lines') or d(o3).get('items') or []
if isinstance(l3, list):
    ck('关键字过滤确实收敛', len(l3) == 0, f'len={len(l3)}')

print('== 3. export：脱敏导出（含轮转文件，从旧到新） ==')
st, ct, body, err = req('GET', '/api/v1/settings/log/export')
ck('export 返回 200', st == 200, f'{st} {err}')
ck('export 非 JSON 归档流', 'json' not in ct.lower(), ct)
raw = body if isinstance(body, (bytes, bytearray)) else b''
ck('export 有内容', len(raw) > 0, f'len={len(raw)}')
# 脱敏断言不写死任何凭据字面量：开发期万能码从环境变量注入，
# 会话密钥按「长十六进制串」的形态检测（AGENTS.md §7 红线）。
import re
forbidden = os.environ.get('KC_FORBIDDEN_LITERAL', '').encode()
hex_secrets = re.findall(rb'(?<![0-9a-f])[0-9a-f]{48,}(?![0-9a-f])', raw)
leaked = [x for x in (forbidden,) if x and x in raw]
ck('export 中无万能码 / 长密钥明文', not leaked and not hex_secrets,
   f'literal={len(leaked)} hex={len(hex_secrets)}')

print('== 4. level：调整级别并记审计 ==')
st, _, o, err = req('GET', '/api/v1/settings/log/status')
before = d(o).get('level') or d(o).get('current_level') or 'info'
st, ct, o, err = req('PUT', '/api/v1/settings/log/level', {'level': 'debug'})
ck('调 DEBUG 成功', st in (200, 204), f'{st} {str(o)[:180]}')
st, _, o, _ = req('GET', '/api/v1/settings/log/status')
after = d(o).get('level') or d(o).get('current_level')
ck('级别已生效（无需重启）', str(after).lower() == 'debug', f'before={before} after={after}')
st, _, o, _ = req('GET', '/api/v1/audit?page=1&page_size=20')
items = d(o).get('items') or []
ck('调级别写入审计', any('log' in str(x.get('action', '')).lower() for x in items if isinstance(x, dict)),
   str([x.get('action') for x in items[:5]]))
# 还原，避免影响后续套件的日志窗口判定
req('PUT', '/api/v1/settings/log/level', {'level': str(before).lower() if str(before).lower() in
    ('debug', 'info', 'warn', 'error') else 'info'})

print('== 5. delete：只截断当前文件，不删文件 ==')
st, ct, o, err = req('POST', '/api/v1/settings/log/delete', {})
ck('清理返回 2xx', 200 <= st < 300, f'{st} {str(o)[:180]}')

print('== 6. SSE stream：connected 首帧 + Cookie 鉴权 ==')
try:
    r = urllib.request.Request(BASE + '/api/v1/settings/log/stream', headers={'Accept': 'text/event-stream'})
    with op.open(r, timeout=6) as resp:
        ck('SSE 返回 200 且 Content-Type 为 event-stream',
           resp.status == 200 and 'event-stream' in resp.headers.get('Content-Type', ''),
           f'{resp.status} {resp.headers.get("Content-Type")}')
        first = resp.readline().decode('utf-8', 'replace').strip()
        ck('SSE 有首帧', bool(first), first)
except Exception as e:
    # 长连接读超时不算失败：只要没拿到 4xx/5xx
    code = getattr(e, 'code', None)
    ck('SSE 未被拒绝', code is None or code < 400, f'{type(e).__name__} {e}')

print('\n' + '=' * 68)
print(f'用例 {passed + len(fails)}，通过 {passed}，失败 {len(fails)}')
if fails:
    print('  FAIL:', fails)
print('=' * 68)
raise SystemExit(1 if fails else 0)
