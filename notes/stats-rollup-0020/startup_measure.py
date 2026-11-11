#!/usr/bin/env python3
"""startup_measure.py —— 测量「首次启动（含 0020 历史回填）到可服务」的耗时与峰值内存。

方法：
  * 先把 stats_backfill_state 清空（= 存量库首次升级后启动的真实状态）；
  * 起被测二进制，每 1s 探 /healthz，每 5s 读 /proc/<pid>/status 的 VmHWM（进程峰值 RSS）；
  * /healthz 首次 200 即「可服务」，记录 wall 时间；随后打一次 /auth/me/summary 确认真的能查；
  * 同一进程内测完即杀，输出 JSON。
"""
import json
import os
import signal
import subprocess
import sys
import time
import urllib.request


def main():
    binary, config, port, token_file, out = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4], sys.argv[5]
    budget = float(sys.argv[6]) if len(sys.argv) > 6 else 4800.0
    token = open(token_file).read().strip()

    log = open(f"logs/startup-{os.getpid()}.log", "ab")
    t0 = time.time()
    p = subprocess.Popen([binary, "-config", config], stdout=log, stderr=log,
                         start_new_session=True)

    peak_kb = 0
    ready_at = None
    last_sample = 0
    while time.time() - t0 < budget:
        try:
            with open(f"/proc/{p.pid}/status") as f:
                for line in f:
                    if line.startswith("VmHWM:"):
                        peak_kb = max(peak_kb, int(line.split()[1]))
                        break
        except Exception:
            pass
        if p.poll() is not None:
            break
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=3) as r:
                if r.status == 200:
                    ready_at = time.time() - t0
                    break
        except Exception:
            pass
        time.sleep(1.0)

    result = dict(elapsed_ready_s=None if ready_at is None else round(ready_at, 1),
                  peak_rss_mb=round(peak_kb / 1024.0, 1), budget_s=budget,
                  pid=p.pid, finished=ready_at is not None)

    if ready_at is not None:
        try:
            req = urllib.request.Request(f"http://127.0.0.1:{port}/api/v1/auth/me/summary",
                                         headers={"Authorization": f"Bearer {token}"})
            ts = time.time()
            with urllib.request.urlopen(req, timeout=600) as r:
                body = r.read()
            result["summary_ok"] = True
            result["summary_ms"] = round((time.time() - ts) * 1000.0, 1)
            result["summary_body"] = body.decode("utf-8", "replace")[:300]
        except Exception as e:
            result["summary_ok"] = False
            result["summary_err"] = str(e)

    try:
        os.killpg(os.getpgid(p.pid), signal.SIGKILL)
    except Exception:
        pass
    with open(out, "a") as f:
        f.write(json.dumps(result, ensure_ascii=False) + "\n")
    print(json.dumps(result, ensure_ascii=False))


if __name__ == "__main__":
    main()
