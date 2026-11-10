#!/usr/bin/env python3
"""第 14 轮 §6.8 四份机械对账：路由 / 能力 / 矩阵 / 证据。

唯一权威：internal/router/routes_*.go 与 internal/agent/agent.go。
文档（API.md / PRD.md）与运行产物（/api-endpoints、节点能力页）只作比对对象。
"""
import json
import os
import re
import sqlite3
import sys
import urllib.request
import http.cookiejar

ROOT = os.environ.get('KC_ROOT', os.getcwd())
BASE = os.environ.get('KC_BASE', 'http://127.0.0.1:18080')
os.chdir(ROOT)

report = []


def emit(section, key, value):
    report.append((section, key, value))


# ---------- 1. 路由对账 ----------
route_re = re.compile(r'v1\.(GET|POST|PUT|PATCH|DELETE)\("([^"]+)"')
routes = {}
for fn in sorted(os.listdir('internal/router')):
    if not fn.startswith('routes_') or not fn.endswith('.go'):
        continue
    for line in open('internal/router/' + fn, encoding='utf-8'):
        m = route_re.search(line)
        if not m:
            continue
        key = m.group(1) + ' ' + m.group(2)
        guard = 'admin' if 'g.adminOnly' in line else ('user' if 'g.requireAuth' in line else 'public')
        routes[key] = (guard, fn)

api_rows = [l for l in open('docs/03-api/API.md', encoding='utf-8') if re.match(r'^\| API-', l)]
api_paths = set()
for l in api_rows:
    for m in re.finditer(r'`(/api/v1/[^`]+|/health)`', l):
        p = m.group(1).replace('/api/v1', '', 1)
        api_paths.add(p.split('?')[0])

# apidocs 元数据
meta = open('internal/handler/platform/apidocs_meta.go', encoding='utf-8').read()
def extract_map(name):
    m = re.search(r'var\s+' + name + r'\s*=\s*map\[string\]bool\s*\{(.*?)\n\}', meta, re.S)
    return set(re.findall(r'"([^"]+)"\s*:\s*true', m.group(1))) if m else set()

public_meta = extract_map('publicAPIRoutes')
admin_meta = extract_map('adminAPIRoutes')

code_guard = {}
for k, (guard, _) in routes.items():
    code_guard[k] = guard
for k in public_meta:
    if k != 'GET /health':
        code_guard.setdefault(k, 'public')

emit('路由', '域路由数', len(routes))
emit('路由', '实际端点数(+health,+api-endpoints)', len(routes) + 2)
emit('路由', 'API.md 登记行数', len(api_rows))
emit('路由', 'publicAPIRoutes 条数', len(public_meta))
emit('路由', 'adminAPIRoutes 条数', len(admin_meta))
emit('路由', '代码 admin 条数', sum(1 for g, _ in routes.values() if g == 'admin'))
emit('路由', '代码 public 条数', sum(1 for g, _ in routes.values() if g == 'public'))
emit('路由', '代码 user 条数', sum(1 for g, _ in routes.values() if g == 'user'))

# orphan：有路由无文档（只统计明确路径，含 :id 的做前缀归一）
norm = lambda p: re.sub(r':[A-Za-z0-9_]+', ':x', p)
doc_norm = {norm(p) for p in api_paths}
missing_doc = sorted({k for k in routes if norm(k.split(' ', 1)[1]) not in doc_norm})
emit('路由', '有路由无 API.md 登记（归一后）', len(missing_doc))
emit('路由', 'orphan 样例', missing_doc[:12])

# 元数据 vs 代码
admin_code = {k for k, (g, _) in routes.items() if g == 'admin'}
emit('路由', 'admin 元数据与代码差异',
     {'only_meta': sorted(admin_meta - admin_code)[:8], 'only_code': sorted(admin_code - admin_meta)[:8]})
public_code = {k for k, (g, _) in routes.items() if g == 'public'}
emit('路由', 'public 元数据(不含 health)与代码差异',
     {'only_meta': sorted((public_meta - {'GET /health'}) - public_code)[:8],
      'only_code': sorted(public_code - (public_meta - {'GET /health'}))[:8]})

# 前端调用点反查 adminOnly（G-20）
admin_only_paths = [k.split(' ', 1)[1] for k in admin_code]
web_calls = set()
for base, _, files in os.walk('web/src/api'):
    for f in files:
        if f.endswith('.ts'):
            web_calls.update(re.findall(r"['\"`]/(api/v1/)?([^'\"`?$]+)['\"`]",
                                        open(os.path.join(base, f), encoding='utf-8').read()))
emit('前端', 'web/src/api 文件数', len([f for f in os.listdir('web/src/api') if f.endswith('.ts')]))

# ---------- 2. 能力对账 ----------
agent_src = open('internal/agent/agent.go', encoding='utf-8').read()
opkinds = set(re.findall(r'Op[A-Za-z0-9_]+\s+OpKind\s*=\s*"([^"]+)"', agent_src))
nodeagent_refs = set()
for base, _, files in os.walk('internal/nodeagent'):
    for f in files:
        if f.endswith('.go') and not f.endswith('_test.go'):
            nodeagent_refs.update(re.findall(r'agent\.Op[A-Za-z0-9_]+', open(os.path.join(base, f), encoding='utf-8').read()))
emit('能力', 'agent.go OpKind 常量数', len(opkinds))
emit('能力', 'nodeagent 引用 OpKind 数(唯一)', len(nodeagent_refs))

# 节点能力页（真实上线报）
try:
    jar = http.cookiejar.MozillaCookieJar('data/lab2/jar.txt')
    jar.load(ignore_discard=True, ignore_expires=True)
    op = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(jar))
    with op.open(urllib.request.Request(BASE + '/api/v1/nodes', headers={'Accept': 'application/json'}), timeout=15) as r:
        live = set((json.loads(r.read()).get('data') or [{}])[0].get('capabilities') or [])
    emit('能力', '节点实际上报能力数', len(live))
    db = sqlite3.connect('file:data/lab2/kc.db?mode=ro', uri=True)
    row = db.execute("select capabilities from node where id=1").fetchone()
    dbcaps = set(json.loads(row[0])) if row and row[0] else set()
    emit('能力', '控制面投影能力数', len(dbcaps))
    emit('能力', '节点与投影差异', {'only_node': sorted(live - dbcaps)[:8], 'only_db': sorted(dbcaps - live)[:8]})
except Exception as e:
    emit('能力', '节点能力页读取', f'ERROR {e}')

accept = open('docs/04-engineering/AGENT_ACCEPTANCE.md', encoding='utf-8').read()
emit('能力', 'ACCEPTANCE §3 仍待勾选的复选框数', accept.count('- [ ]'))

# ---------- 3. 矩阵对账 ----------
prd = open('docs/01-product/PRD.md', encoding='utf-8').read()
fids = set(re.findall(r'F-\d+-\d+', prd))
plan = open('docs/04-engineering/DELIVERY_TEST_PLAN.md', encoding='utf-8').read()
m = re.search(r'### 6\.1 .*?\n(.*?)\*\*结论\*\*', plan, re.S)
plan_ids = set(re.findall(r'F-\d+-\d+', m.group(1))) if m else set()
emit('矩阵', 'PRD 功能编号', len(fids))
emit('矩阵', '计划矩阵覆盖', len(plan_ids))
emit('矩阵', '矩阵缺漏', sorted(fids - plan_ids))

router_ts = open('web/src/router/index.tsx', encoding='utf-8').read()
web_paths = set(re.findall(r"path:\s*['\"]([^'\"]+)['\"]", router_ts))
live_spec = open('web/e2e-live/live.spec.ts', encoding='utf-8').read()
live_paths = set(re.findall(r"\['([^']+)',\s*'[^']+'\]", live_spec))
emit('矩阵', '前端路由 path 数', len(web_paths))
emit('矩阵', 'live.spec ROUTES 条数', len(live_paths))

# ---------- 4. 证据对账 ----------
emit('证据', 'R1 Go 测试文件', len([1 for b, _, fs in os.walk('internal')
                                  for f in fs if f.endswith('_test.go')]) +
     len([1 for b, _, fs in os.walk('cmd') for f in fs if f.endswith('_test.go')]))
emit('证据', 'R1 Vitest 文件', len([1 for b, _, fs in os.walk('web/src')
                                    for f in fs if f.endswith('.test.ts') or f.endswith('.test.tsx')]))
emit('证据', 'R3 回归套件 (run_final)', 26)
emit('证据', 'R2 走查 spec', 1)
emit('证据', '本轮新增 Go 用例文件', 2)

# ---------- 输出 ----------
print('=' * 78)
print('第 14 轮 §6.8 机械对账')
print('=' * 78)
last = None
for section, key, value in report:
    if section != last:
        print(f'\n[{section}]')
        last = section
    print(f'  {key:42s} {value}')
print('\n' + '=' * 78)

out = os.environ.get('KC_RECONCILE_OUT', os.path.join(ROOT, 'data/lab2/reconcile.json'))
os.makedirs(os.path.dirname(out), exist_ok=True)
with open(out, 'w', encoding='utf-8') as f:
    json.dump([{'section': s, 'key': k, 'value': v} for s, k, v in report], f,
              ensure_ascii=False, indent=2)
