#!/usr/bin/env python3
"""渠道 demo：模拟上游渠道对 OpenAI 兼容请求的响应。

同一进程按 URL 路径前缀模拟多种渠道行为，供平台配置的渠道指向不同 profile：
  /v1/...            normal      200，返回合法 usage
  /slow/v1/...       slow        注入延迟后 200（抬高 P99）
  /ratelimited/v1/...ratelimited 约 50% 返回 429
  /dead/v1/...       dead        恒定 401（凭据失效 → 渠道禁用）
  /broken/v1/...     broken      恒定 500

平台里的渠道 BaseURL 只需写成 http://127.0.0.1:9099/<profile>/v1，
探测会 POST 到 <BaseURL>/chat/completions，即落到本服务。
"""
import json
import random
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

HOST = "127.0.0.1"
PORT = 9099


def profile_of(path: str) -> str:
    p = path.lower()
    for name in ("dead", "broken", "ratelimited", "slow"):
        if f"/{name}/" in p or p.rstrip("/").endswith(f"/{name}") or p.startswith(f"/{name}"):
            return name
    return "normal"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # 精简日志
        sys.stderr.write("[mock] %s %s\n" % (self.path, fmt % args))

    def _send(self, code: int, payload: dict):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _read_body(self) -> dict:
        n = int(self.headers.get("Content-Length") or 0)
        if not n:
            return {}
        raw = self.rfile.read(n)
        try:
            return json.loads(raw.decode() or "{}")
        except Exception:
            return {}

    def do_GET(self):
        path = urlparse(self.path).path
        if path.rstrip("/").endswith("/models"):
            self._send(200, {
                "object": "list",
                "data": [{"id": "demo-model-a", "object": "model", "owned_by": "demo"}],
            })
            return
        self._send(404, {"error": {"message": "not found", "type": "invalid_request_error"}})

    def do_POST(self):
        path = urlparse(self.path).path
        prof = profile_of(path)
        body = self._read_body()
        model = body.get("model", "demo-model")

        if prof == "dead":
            self._send(401, {"error": {"message": "invalid api key", "type": "authentication_error"}})
            return
        if prof == "broken":
            self._send(500, {"error": {"message": "internal upstream error", "type": "server_error"}})
            return
        if prof == "ratelimited":
            if random.random() < 0.5:
                self._send(429, {"error": {"message": "rate limit exceeded", "type": "rate_limit_error"}})
                return
        if prof == "slow":
            time.sleep(1.2)

        prompt = random.choice([8, 12, 20])
        completion = random.choice([2, 3, 5])
        self._send(200, {
            "id": "chatcmpl-mock-" + str(int(time.time() * 1000)),
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "pong"},
                "finish_reason": "stop",
            }],
            "usage": {
                "prompt_tokens": prompt,
                "completion_tokens": completion,
                "total_tokens": prompt + completion,
            },
        })

    def do_PUT(self):
        self._send(404, {"error": {"message": "not found"}})


def main():
    srv = ThreadingHTTPServer((HOST, PORT), Handler)
    sys.stderr.write(f"[mock] channel demo listening on http://{HOST}:{PORT}\n")
    srv.serve_forever()


if __name__ == "__main__":
    main()
