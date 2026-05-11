#!/usr/bin/env python3
"""
06_python_client.py — 非 Go 服务接入 OAuth2 示例 (Python 版)。

依赖: pip install requests

跑:
   source .env && python3 06_python_client.py
"""
import os
import time
import json
import requests
from typing import Optional


class OAuth2Client:
    """轻量 client_credentials SDK — auto-refresh + 缓存。
    可直接 copy 到你的 Python 服务用。
    """

    def __init__(self, token_url: str, client_id: str, client_secret: str,
                 scope: str = "", refresh_skew_sec: int = 300):
        self.token_url = token_url
        self.client_id = client_id
        self.client_secret = client_secret
        self.scope = scope
        self.refresh_skew = refresh_skew_sec
        self._token: Optional[str] = None
        self._expires_at: float = 0

    def token(self) -> str:
        """返当前有效 token (自动 refresh)."""
        if self._token and time.time() + self.refresh_skew < self._expires_at:
            return self._token
        return self._fetch()

    def _fetch(self) -> str:
        data = {
            "grant_type": "client_credentials",
            "client_id": self.client_id,
            "client_secret": self.client_secret,
        }
        if self.scope:
            data["scope"] = self.scope
        resp = requests.post(self.token_url, data=data, timeout=5)
        resp.raise_for_status()
        body = resp.json()
        self._token = body["access_token"]
        self._expires_at = time.time() + body["expires_in"]
        return self._token

    def request(self, method: str, url: str, **kwargs) -> requests.Response:
        """统一发请求 — 自动加 Bearer header."""
        headers = kwargs.pop("headers", {}) or {}
        headers["Authorization"] = f"Bearer {self.token()}"
        return requests.request(method, url, headers=headers, **kwargs)


def main():
    oauth_host = os.environ["OAUTH_HOST"]
    api_base = os.environ.get("API_BASE", "http://localhost:9090")

    print("=" * 50)
    print(" Python OAuth2 集成 Demo")
    print("=" * 50)

    # 用商户 client
    tc = OAuth2Client(
        token_url=f"{oauth_host}/oauth2/token",
        client_id=os.environ["MER_CLIENT_ID"],
        client_secret=os.environ["MER_CLIENT_SECRET"],
        scope="charge:write refund:write",
    )

    print(f"\n▶ [1] 创建 charge")
    r = tc.request("POST", f"{api_base}/api/v1/charges",
                   json={"amount_minor": 200, "currency": "USD"})
    print(f"  HTTP {r.status_code}")
    print(f"  {json.dumps(r.json(), indent=2)}")

    print(f"\n▶ [2] /whoami (验证 actor)")
    r = tc.request("GET", f"{api_base}/api/v1/whoami")
    print(f"  HTTP {r.status_code}")
    print(f"  {json.dumps(r.json(), indent=2)}")

    print(f"\n▶ [3] 创建 refund")
    r = tc.request("POST", f"{api_base}/api/v1/refunds",
                   json={"charge_id": "ch_demo", "amount_minor": 50})
    print(f"  HTTP {r.status_code}")
    print(f"  {json.dumps(r.json(), indent=2)}")

    print(f"\n▶ [4] 用 introspect 验签 (非 Go 服务推荐这种集成方式)")
    intro = requests.post(
        f"{oauth_host}/oauth2/introspect",
        data={
            "token": tc.token(),
            "client_id": tc.client_id,
            "client_secret": tc.client_secret,
        },
        timeout=5,
    )
    print(f"  HTTP {intro.status_code}")
    print(f"  {json.dumps(intro.json(), indent=2)}")

    print(f"\n✅ Python demo 完成")


if __name__ == "__main__":
    main()
