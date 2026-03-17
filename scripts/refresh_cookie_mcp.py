#!/usr/bin/env python3
"""
Refresh OUTLIER_COOKIE / OUTLIER_USER_AGENT in .env via Playwright MCP extension transport.

Example:
  set PLAYWRIGHT_MCP_EXTENSION_TOKEN=...
  python scripts/refresh_cookie_mcp.py --env .env

Daemon mode:
  python scripts/refresh_cookie_mcp.py --daemon --lead-seconds 3600
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import random
import re
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import requests


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Refresh Outlier cookie via Playwright MCP extension")
    parser.add_argument("--token", default=os.getenv("PLAYWRIGHT_MCP_EXTENSION_TOKEN", ""), help="Playwright MCP extension token")
    parser.add_argument("--env", type=Path, default=Path(".env"), help="Path to .env")
    parser.add_argument("--url", default=os.getenv("OUTLIER_URL", "https://playground.outlier.ai/"), help="Target URL for cookie scope")
    parser.add_argument("--host", default=os.getenv("PLAYWRIGHT_MCP_HOST", "localhost"), help="MCP bind host")
    parser.add_argument("--port", type=int, default=0, help="MCP bind port (0 => random)")
    parser.add_argument("--start-timeout", type=int, default=30, help="MCP startup timeout in seconds")
    parser.add_argument("--lead-seconds", type=int, default=3600, help="Refresh lead time before expiry in daemon mode")
    parser.add_argument("--poll-seconds", type=int, default=300, help="Fallback poll interval in daemon mode")
    parser.add_argument("--daemon", action="store_true", help="Run forever and refresh before expiry")
    parser.add_argument("--debug", action="store_true", help="Enable debug logs")
    return parser.parse_args()


def decode_jwt_exp(jwt_value: str) -> int:
    if not jwt_value:
        return 0
    parts = jwt_value.split(".")
    if len(parts) < 2:
        return 0
    payload = parts[1].replace("-", "+").replace("_", "/")
    payload += "=" * ((4 - len(payload) % 4) % 4)
    try:
        obj = json.loads(base64.b64decode(payload).decode("utf-8"))
        exp = int(float(obj.get("exp", 0)))
        return exp if exp > 0 else 0
    except Exception:
        return 0


def upsert_env_line(text: str, key: str, value: str) -> str:
    escaped = value.replace("\\", "\\\\").replace('"', '\\"')
    line = f'{key}="{escaped}"'
    pattern = re.compile(rf"^\s*{re.escape(key)}=.*$", re.MULTILINE)
    if pattern.search(text):
        return pattern.sub(line, text)
    if text and not text.endswith("\n"):
        text += "\n"
    return text + line + "\n"


def update_env_file(env_file: Path, cookie_header: str, user_agent: str) -> None:
    try:
        current = env_file.read_text(encoding="utf-8")
    except FileNotFoundError:
        current = ""
    current = upsert_env_line(current, "OUTLIER_COOKIE", cookie_header)
    current = upsert_env_line(current, "OUTLIER_USER_AGENT", user_agent)
    env_file.write_text(current, encoding="utf-8")


def parse_sse_messages(raw: str) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for line in raw.splitlines():
        if not line.startswith("data: "):
            continue
        data = line[6:].strip()
        if not data:
            continue
        try:
            out.append(json.loads(data))
        except Exception:
            pass
    return out


def extract_tool_text(message: dict[str, Any] | None) -> str:
    if not message:
        return ""
    content = message.get("result", {}).get("content", [])
    texts = [item.get("text", "") for item in content if isinstance(item, dict) and item.get("type") == "text"]
    return "\n".join(t for t in texts if isinstance(t, str))


def parse_result_json(tool_text: str) -> dict[str, Any]:
    if not tool_text.strip():
        raise RuntimeError("empty tool result")
    m = re.search(r"### Result\s*\n([\s\S]*?)(?:\n### |\Z)", tool_text)
    payload = (m.group(1) if m else tool_text).strip()
    payload = re.sub(r"^```json\s*", "", payload, flags=re.IGNORECASE)
    payload = re.sub(r"^```\s*", "", payload)
    payload = re.sub(r"\s*```$", "", payload)
    payload = payload.strip()
    if not payload:
        raise RuntimeError("empty result payload")
    try:
        parsed = json.loads(payload)
    except Exception as exc:
        raise RuntimeError(f"failed to parse result JSON: {payload[:240]}") from exc
    if not isinstance(parsed, dict):
        raise RuntimeError("result payload is not a JSON object")
    return parsed


def min_positive(*vals: int | float | None) -> int:
    candidates = []
    for v in vals:
        try:
            iv = int(float(v or 0))
        except Exception:
            iv = 0
        if iv > 0:
            candidates.append(iv)
    return min(candidates) if candidates else 0


@dataclass
class CaptureResult:
    cookie_header: str
    user_agent: str
    cookie_count: int
    next_expiry: int


class MCPServer:
    def __init__(self, token: str, host: str, port: int, debug: bool = False):
        self.token = token
        self.host = host
        self.port = port
        self.debug = debug
        self.proc: subprocess.Popen[str] | None = None
        self.log_path = Path(tempfile.gettempdir()) / f"outlier_mcp_{os.getpid()}_{self.port}.log"
        self.base_url = f"http://{self.host}:{self.port}/mcp"

    def start(self) -> None:
        cmd = f"npx @playwright/mcp@latest --extension --ignore-https-errors --host {self.host} --port {self.port}"
        env = dict(os.environ)
        env["PLAYWRIGHT_MCP_EXTENSION_TOKEN"] = self.token
        log_file = self.log_path.open("w", encoding="utf-8")
        self.proc = subprocess.Popen(
            ["cmd.exe", "/d", "/s", "/c", cmd],
            stdout=log_file,
            stderr=subprocess.STDOUT,
            text=True,
            env=env,
        )

    def stop(self) -> None:
        if not self.proc:
            return
        if self.proc.poll() is None:
            if os.name == "nt":
                subprocess.run(
                    ["taskkill", "/PID", str(self.proc.pid), "/T", "/F"],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    check=False,
                )
            else:
                self.proc.terminate()
        self.proc = None

    def logs(self) -> str:
        try:
            return self.log_path.read_text(encoding="utf-8")
        except Exception:
            return ""

    def ensure_up(self, timeout_seconds: int) -> None:
        start = time.time()
        while time.time() - start < timeout_seconds:
            if self.proc and self.proc.poll() is not None:
                raise RuntimeError(f"MCP process exited early (code={self.proc.returncode})")
            try:
                requests.get(self.base_url, timeout=0.8)
            except Exception:
                pass
            try:
                r = requests.post(
                    self.base_url,
                    headers={"Accept": "application/json, text/event-stream", "Content-Type": "application/json"},
                    json={
                        "jsonrpc": "2.0",
                        "id": 1,
                        "method": "initialize",
                        "params": {
                            "protocolVersion": "2024-11-05",
                            "capabilities": {},
                            "clientInfo": {"name": "startup-probe", "version": "1.0.0"},
                        },
                    },
                    timeout=5,
                )
                if r.status_code == 200:
                    return
            except Exception:
                pass
            time.sleep(0.2)
        raise RuntimeError(f"timeout waiting for MCP on {self.base_url}")


def mcp_post(base_url: str, payload: dict[str, Any], session_id: str = "", timeout: int = 30, debug: bool = False) -> tuple[int, str, dict[str, Any] | None, str]:
    headers = {
        "Accept": "application/json, text/event-stream",
        "Content-Type": "application/json",
    }
    if session_id:
        headers["mcp-session-id"] = session_id
    resp = requests.post(base_url, headers=headers, json=payload, timeout=timeout)
    text = resp.text or ""
    sid_out = resp.headers.get("mcp-session-id", session_id)
    if debug:
        print(
            f"[debug] method={payload.get('method')} status={resp.status_code} req_sid={session_id or '-'} "
            f"resp_sid={sid_out or '-'} len={len(text)}"
        )
    if resp.status_code not in (200, 202):
        raise RuntimeError(f"MCP {payload.get('method')} failed status={resp.status_code} body={text[:500]}")
    msg = None
    events = parse_sse_messages(text)
    if events:
        msg = events[-1]
    return resp.status_code, sid_out, msg, text


def capture_once(args: argparse.Namespace) -> CaptureResult:
    port = args.port or random.randint(18000, 27999)
    server = MCPServer(token=args.token, host=args.host, port=port, debug=args.debug)
    server.start()
    try:
        server.ensure_up(args.start_timeout)

        _, sid, _, _ = mcp_post(
            server.base_url,
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2024-11-05",
                    "capabilities": {},
                    "clientInfo": {"name": "outlier-cookie-refresh", "version": "1.0.0"},
                },
            },
            debug=args.debug,
        )
        if not sid:
            raise RuntimeError("missing mcp-session-id after initialize")

        mcp_post(
            server.base_url,
            {"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}},
            session_id=sid,
            debug=args.debug,
        )

        js_code = (
            "async (page) => { "
            f"const cookies = await page.context().cookies(\"{args.url}\"); "
            "const userAgent = await page.evaluate(() => navigator.userAgent); "
            "return { userAgent, cookies: cookies.map(c => ({ name: c.name, value: c.value, expires: Number(c.expires || 0) })), "
            "cookieHeader: cookies.map(c => `${c.name}=${c.value}`).join('; ') }; }"
        )
        _, sid2, message, raw = mcp_post(
            server.base_url,
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {"name": "browser_run_code", "arguments": {"code": js_code}},
            },
            session_id=sid,
            timeout=45,
            debug=args.debug,
        )
        if args.debug:
            print(f"[debug] sid_after_tool={sid2}")
            print(f"[debug] raw_preview={raw[:400]}")

        tool_text = extract_tool_text(message)
        result = parse_result_json(tool_text)

        cookie_header = str(result.get("cookieHeader", "")).strip()
        user_agent = str(result.get("userAgent", "")).strip()
        cookies = result.get("cookies", [])
        if not cookie_header:
            raise RuntimeError("cookieHeader missing in MCP result")
        if not user_agent:
            raise RuntimeError("userAgent missing in MCP result")
        if not isinstance(cookies, list):
            cookies = []

        jwt = next((c for c in cookies if isinstance(c, dict) and c.get("name") == "_jwt"), {})
        sess = next((c for c in cookies if isinstance(c, dict) and c.get("name") == "_session"), {})
        next_expiry = min_positive(jwt.get("expires"), sess.get("expires"), decode_jwt_exp(str(jwt.get("value", ""))))

        return CaptureResult(
            cookie_header=cookie_header,
            user_agent=user_agent,
            cookie_count=len(cookies) if cookies else len([x for x in cookie_header.split("; ") if x]),
            next_expiry=next_expiry,
        )
    except Exception as exc:
        logs = server.logs().strip()
        if logs:
            raise RuntimeError(f"{exc}\n[mcp logs]\n{logs}") from exc
        raise
    finally:
        server.stop()


def run_once(args: argparse.Namespace) -> CaptureResult:
    result = capture_once(args)
    update_env_file(args.env, result.cookie_header, result.user_agent)
    expiry_text = datetime.fromtimestamp(result.next_expiry, tz=timezone.utc).isoformat() if result.next_expiry > 0 else "unknown"
    print(
        f"[refresh_cookie_mcp] updated {args.env} "
        f"cookies={result.cookie_count} next_expiry={expiry_text}"
    )
    return result


def main() -> int:
    args = parse_args()
    if not args.token:
        raise RuntimeError("missing token: set PLAYWRIGHT_MCP_EXTENSION_TOKEN or pass --token")
    if args.start_timeout <= 0:
        raise RuntimeError("--start-timeout must be > 0")
    if args.poll_seconds <= 0:
        raise RuntimeError("--poll-seconds must be > 0")
    if args.lead_seconds < 0:
        raise RuntimeError("--lead-seconds must be >= 0")

    if not args.daemon:
        run_once(args)
        return 0

    while True:
        result = run_once(args)
        wait_seconds = args.poll_seconds
        if result.next_expiry > 0:
            wait_seconds = max(30, result.next_expiry - int(time.time()) - args.lead_seconds)
        print(f"[refresh_cookie_mcp] sleep {wait_seconds}s")
        time.sleep(wait_seconds)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("[refresh_cookie_mcp] stopped")
        raise SystemExit(130)
    except Exception as exc:
        print(f"[refresh_cookie_mcp] {exc}", file=sys.stderr)
        raise SystemExit(1)
