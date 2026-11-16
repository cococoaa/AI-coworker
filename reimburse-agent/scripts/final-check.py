import subprocess, os, time, json, urllib.request, sys

ROOT = r"D:\虹桥黑客松\reimburse-agent"
D = os.path.join(ROOT, "dist")
EXE = os.path.join(D, "reimburse-agent.exe")
TOK = "final-check-token-9a8b7c6d5e"
BASE = "http://172.20.188.82:8799"

subprocess.run(["taskkill", "/F", "/IM", "reimburse-agent.exe"], capture_output=True)
time.sleep(1)

env = dict(os.environ)
env["REIMBURSE_UI_TOKENS"] = "guest:" + TOK
out = open(os.path.join(D, "final-check.out"), "w", encoding="utf-8")
p = subprocess.Popen([EXE, "-addr", "0.0.0.0:8799", "-demo-synthetic"],
                     cwd=D, env=env, stdout=out, stderr=out)
time.sleep(2.5)

H = {"Authorization": "Bearer " + TOK}


def req(path, data=None):
    r = urllib.request.Request(BASE + path, data=data, headers=dict(H))
    if data is not None:
        r.add_header("Content-Type", "application/json")
    return json.loads(urllib.request.urlopen(r, timeout=10).read())


meta = req("/api/meta")
print("用例", meta["cases"], "规则", meta["rules"]["count"], "判定项", meta["rules"]["checks"], "工具", meta["tools"]["mode"])

for cid in ["C01", "C08", "C24", "C36", "C49", "C51"]:
    sid = req("/api/audit", json.dumps({"case_id": cid}).encode())["session_id"]
    d = None
    for _ in range(100):
        time.sleep(0.1)
        d = req("/api/audit/" + sid)
        if d["done"]:
            break
    print(cid, d["report"]["result"]["decision"], d["report"]["result"]["hits"], len(d["events"]), "events")

p.terminate()
print("done")
