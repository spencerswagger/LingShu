#!/usr/bin/env python3
"""consistency.py —— 同一 key 连续 10 次调用结果是否逐字节一致（去掉每次变化的 RequestID）。

用途：证明「进程内缓存」不会让同一 key 前后结果漂移（首调用 miss、后 9 次 hit 的值必须相同）。
"""
import hashlib
import json
import os
import signal
import subprocess
import sys
import time
import urllib.request

ENDPOINTS = {
    "cost": "/api/v1/admin/cost/stats",
    "summary": "/api/v1/auth/me/summary",
}


def start(binary, config, logpath):
    log = open(logpath, "ab")
    return subprocess.Popen([binary, "-config", config], stdout=log, stderr=log, start_new_session=True)


def wait_ready(port, timeout=600):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=3) as r:
                if r.status == 200:
                    return True
        except Exception:
            pass
        time.sleep(0.2)
    return False


def call(port, path, token):
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", headers={"Authorization": f"Bearer {token}"})
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=1800) as r:
        body = json.loads(r.read())
        cache = r.headers.get("x-stats-cache", "")
    ms = (time.perf_counter() - t0) * 1000
    body.pop("RequestID", None)          # 每次请求都不同，不参与一致性比对
    payload = json.dumps(body, sort_keys=True, ensure_ascii=False)
    return ms, cache, payload


def main():
    binary, config, port, token_file, endpoint = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4], sys.argv[5]
    n = int(sys.argv[6]) if len(sys.argv) > 6 else 10
    token = open(token_file).read().strip()
    path = ENDPOINTS[endpoint]

    p = start(binary, config, f"logs/consistency-{os.getpid()}.log")
    try:
        if not wait_ready(port):
            print("ERROR: not ready", file=sys.stderr)
            return 2
        rows = []
        for i in range(n):
            ms, cache, payload = call(port, path, token)
            digest = hashlib.sha256(payload.encode()).hexdigest()[:16]
            rows.append((i + 1, round(ms, 1), cache, digest, len(payload)))
        digests = {r[3] for r in rows}
        print(f"endpoint={endpoint} calls={n} distinct_digests={len(digests)} identical={len(digests) == 1}")
        for r in rows:
            print(f"  #{r[0]:>2} {r[1]:>10.1f}ms cache={r[2]:<5} sha={r[3]} bytes={r[4]}")
        print("payload_head=", payload[:200])
    finally:
        try:
            os.killpg(os.getpgid(p.pid), signal.SIGKILL)
        except Exception:
            pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
