#!/usr/bin/env bash
# Verifies the README quickstart end to end with the containerised app:
#   make up -> make seed -> (the UI's setup flow, driven here through the API with the
#   same "local stack" values the buttons fill in) -> streaming -> make verify
# Prints a short report; exits non-zero on any failure.
set -euo pipefail
cd "$(dirname "$0")/.."
BASE=${LAKESYNC_URL:-http://localhost:8080}
echo "== make up"
make up >/dev/null
for i in $(seq 1 90); do curl -sf "$BASE/readyz" >/dev/null && break; sleep 2; done
curl -sf "$BASE/readyz"; echo
echo "== make seed (initial load only, through the app container)"
docker compose -f deploy/local/docker-compose.yml --profile app run --rm --no-deps lakesync seed --uri "mongodb://mongodb:27017/?replicaSet=rs0" --drop --initial customers=1000,products=200,orders=3000,events=5000 --quiet
echo "== setup flow through the API, with the local stack hints"
python3 - "$BASE" <<'PY'
import json, sys, time, urllib.request, http.cookiejar
BASE=sys.argv[1]
cj=http.cookiejar.CookieJar(); op=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj)); csrf=None
def call(m,p,b=None):
    req=urllib.request.Request(BASE+p,data=json.dumps(b).encode() if b is not None else None,method=m); req.add_header("Content-Type","application/json")
    if csrf: req.add_header("X-CSRF-Token",csrf)
    with op.open(req,timeout=300) as r: return json.loads(r.read().decode())
me=call("POST","/api/auth/login",{"email":"admin@example.com","password":"changeme-now"}); csrf=me["csrf_token"]
hints=me["local_stack_hints"]; assert hints, "LOCAL_STACK_HINTS not set on the container"
src=call("POST","/api/connections",{"kind":"source","type":"mongodb","name":"Local MongoDB",**hints["mongodb"]})
rep=call("POST",f"/api/connections/{src['id']}/test"); assert rep["ok"], rep
dst=call("POST","/api/connections",{"kind":"destination","type":"iceberg_rest","name":"Local Nessie + S3",**hints["iceberg_rest"]})
rep=call("POST",f"/api/connections/{dst['id']}/test"); assert rep["ok"], [ (s["name"],s["status"],s.get("message")) for s in rep["steps"]]
ents=[e for e in call("GET",f"/api/connections/{src['id']}/entities") if e["namespace"]=="shop"]
p=call("POST","/api/pipelines",{"name":"shop","source_id":src["id"],"destination_id":dst["id"],"sync_interval_s":10,"collections":[{"collection":f"{e['namespace']}.{e['name']}","estimated_count":e["estimated_count"]} for e in ents],"start":True})
t0=time.time()
while time.time()-t0<300:
    p=call("GET",f"/api/pipelines/{p['id']}")
    if all(c["state"]=="streaming" for c in p["collections"]): break
    time.sleep(3)
assert all(c["state"]=="streaming" for c in p["collections"]), [(c["collection"],c["state"],c["last_error"]) for c in p["collections"]]
print("   pipeline streaming after %.0fs: %s" % (time.time()-t0, [(c["collection"],c["last_seq"]) for c in p["collections"]]))
rows=call("GET",f"/api/tables/shop/customers/rows?limit=3")
print("   viewer rows:", rows["total"], "columns:", len(rows["columns"]))
q=call("POST","/api/query",{"destination_id":dst["id"],"sql":"SELECT count(*) AS n FROM shop.orders"})
print("   sql console:", q["result"]["rows"])
PY
echo "== make verify (through the app container)"
docker compose -f deploy/local/docker-compose.yml --profile app run --rm --no-deps lakesync verify
echo "== quickstart check passed"
