import json, os, pathlib, sys, time
S = os.environ['SCRATCH']
from optimizer.app import app
import optimizer.optimizer as o
assert o.SYSTEM_CBC and S in o.SYSTEM_CBC, o.SYSTEM_CBC
out = pathlib.Path(sys.argv[1]); out.mkdir(parents=True, exist_ok=True)
client = app.test_client()
cases = sorted(pathlib.Path('test_cases').glob('*.json'))
if len(sys.argv) > 2: cases = [c for c in cases if any(a in c.name for a in sys.argv[2:])]
for case in cases:
    req = json.loads(case.read_text())['request']
    os.environ['CBC_SHIM_CAPTURE'] = str(out / case.stem / 'cap')
    t0 = time.perf_counter()
    r = client.post('/optimize/charge-schedule', json=req)
    el = time.perf_counter() - t0
    (out / case.stem).mkdir(exist_ok=True)
    (out / case.stem / 'response.json').write_text(r.get_data(as_text=True))
    print(f"{case.stem} {r.status_code} {r.json.get('status')} {r.json.get('objective_value')} {el:.3f}s", flush=True)
