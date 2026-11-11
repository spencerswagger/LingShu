#!/usr/bin/env python3
"""bench.py —— 统计/账单接口压测驱动（Gate 独立复核用，2026-11-11）

设计要点（口径可复核）：
  * 黑盒：只打 HTTP，不 import 被测代码，不读内部计数。
  * 冷 / 热两种采样：
      cold —— 每条样本单独起一次被测进程（进程内缓存必然为空 → 必然冷回源）；
      hot  —— 一个进程内 1 次预热 + N 次重复（命中 5 分钟缓存）。
  * 每条样本记录：端点、耗时(ms)、x-stats-cache 头、响应字节数、started_at。
  * 结果逐条 append 成 JSONL，中途被杀也不丢已采到的样本。

用法：
  bench.py --binary ./bin/server-branch --config config-branch.yaml --port 18391 \
           --endpoint cost --mode cold --samples 5 --campaign 1 --out cost-cold.jsonl
"""
import argparse
import json
import os
import signal
import statistics
import subprocess
import sys
import time
import urllib.request

ENDPOINTS = {
    "cost": "/api/v1/admin/cost/stats",
    "summary": "/api/v1/auth/me/summary",
}


def start_server(binary, config, logpath):
    log = open(logpath, "ab")
    p = subprocess.Popen(
        [binary, "-config", config],
        stdout=log, stderr=log, start_new_session=True,
    )
    return p


def kill_server(p):
    try:
        os.killpg(os.getpgid(p.pid), signal.SIGKILL)
    except Exception:
        try:
            p.kill()
        except Exception:
            pass
    try:
        p.wait(timeout=30)
    except Exception:
        pass


def wait_ready(port, timeout=1800):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=3) as r:
                if r.status == 200:
                    return True
        except Exception:
            pass
        time.sleep(0.2)
    return False


def call(port, path, token, timeout=3600):
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}{path}",
        headers={"Authorization": f"Bearer {token}"},
    )
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=timeout) as r:
        body = r.read()
        status = r.status
        cache = r.headers.get("x-stats-cache", "")
    ms = (time.perf_counter() - t0) * 1000.0
    return ms, status, cache, len(body)


def pct(values, q):
    """nearest-rank 分位（小样本下定义明确、可手算复核）。"""
    if not values:
        return float("nan")
    xs = sorted(values)
    k = max(1, int(round(q * len(xs) + 0.4999)))
    return xs[min(k, len(xs)) - 1]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True)
    ap.add_argument("--config", required=True)
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--endpoint", choices=list(ENDPOINTS), required=True)
    ap.add_argument("--mode", choices=["cold", "hot"], required=True)
    ap.add_argument("--samples", type=int, required=True)
    ap.add_argument("--campaign", type=int, required=True)
    ap.add_argument("--token-file", default="admin.jwt")
    ap.add_argument("--out", required=True)
    ap.add_argument("--logdir", default="logs")
    args = ap.parse_args()

    token = open(args.token_file).read().strip()
    path = ENDPOINTS[args.endpoint]
    os.makedirs(args.logdir, exist_ok=True)

    rows = []
    if args.mode == "hot":
        logpath = os.path.join(args.logdir, f"server-{args.endpoint}-hot-c{args.campaign}.log")
        p = start_server(args.binary, args.config, logpath)
        try:
            if not wait_ready(args.port):
                print("ERROR: server not ready", file=sys.stderr)
                return 2
            call(args.port, path, token)  # 预热
            for i in range(args.samples):
                ms, st, cache, n = call(args.port, path, token)
                rows.append(dict(campaign=args.campaign, idx=i, mode="hot",
                                 endpoint=args.endpoint, ms=ms, status=st,
                                 cache=cache, bytes=n, ts=time.time()))
        finally:
            kill_server(p)
    else:
        for i in range(args.samples):
            logpath = os.path.join(args.logdir,
                                   f"server-{args.endpoint}-cold-c{args.campaign}-{i}.log")
            p = start_server(args.binary, args.config, logpath)
            try:
                t_ready = time.time()
                if not wait_ready(args.port):
                    print("ERROR: server not ready", file=sys.stderr)
                    return 2
                ms, st, cache, n = call(args.port, path, token)
                rows.append(dict(campaign=args.campaign, idx=i, mode="cold",
                                 endpoint=args.endpoint, ms=ms, status=st,
                                 cache=cache, bytes=n, ts=time.time(),
                                 startup_s=round(t_ready - 0, 0)))
            finally:
                kill_server(p)

    with open(args.out, "a") as f:
        for r in rows:
            f.write(json.dumps(r, ensure_ascii=False) + "\n")

    xs = [r["ms"] for r in rows]
    print(json.dumps(dict(campaign=args.campaign, endpoint=args.endpoint, mode=args.mode,
                          n=len(xs), samples=[round(x, 1) for x in xs],
                          p50=round(pct(xs, 0.50), 1), p95=round(pct(xs, 0.95), 1),
                          median=round(statistics.median(xs), 1),
                          caches=[r["cache"] for r in rows]), ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
