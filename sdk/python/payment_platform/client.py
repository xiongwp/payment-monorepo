"""Python SDK 主入口. 自动 token / idempotency / retry / webhook sig 验证."""

import hashlib
import hmac
import json
import secrets
import time
from typing import Any, Dict, Optional, Union
from urllib.parse import urlencode
from urllib.request import Request, urlopen
from urllib.error import HTTPError, URLError


API_VERSION = "2026-05-01"


class APIError(Exception):
    """HTTP 错误 — 含 status_code / error_code / body."""
    def __init__(self, message, status_code=None, error_code=None, body=None):
        super().__init__(message)
        self.status_code = status_code
        self.error_code = error_code
        self.body = body or {}


class WebhookSignatureError(Exception):
    pass


class Client:
    def __init__(
        self,
        api_key: str,
        base_url: Optional[str] = None,
        timeout: float = 30.0,
        max_retries: int = 3,
    ):
        if not api_key:
            raise ValueError("api_key required (pk_test_... or pk_live_...)")
        self.api_key = api_key
        # 自动按 key 前缀选 backend
        if api_key.startswith("pk_test_"):
            self.mode = "test"
        elif api_key.startswith("pk_live_"):
            self.mode = "live"
        else:
            self.mode = "live"
        self.base_url = base_url or (
            "https://api-sandbox.payment.example.com" if self.mode == "test"
            else "https://api.payment.example.com"
        )
        self.timeout = timeout
        self.max_retries = max_retries

        self.charges = _ResourceAPI(self, "charges")
        self.refunds = _ResourceAPI(self, "refunds")
        self.payouts = _ResourceAPI(self, "payouts")
        self.customers = _ResourceAPI(self, "customers")
        self.webhooks = _Webhooks(self)

    def request(
        self,
        method: str,
        path: str,
        body: Optional[Dict[str, Any]] = None,
        idempotency_key: Optional[str] = None,
    ) -> Dict[str, Any]:
        url = self.base_url + path
        headers = {
            "Authorization": "Bearer " + self.api_key,
            "User-Agent": f"payment_platform-python/{__import__('payment_platform').__version__}",
            "Accept": "application/json",
            "X-API-Version": API_VERSION,
        }
        data = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(body).encode()
        # 写操作幂等 key
        if method.upper() not in ("GET", "HEAD"):
            headers["Idempotency-Key"] = idempotency_key or "idem_" + secrets.token_hex(16)

        last_err = None
        for attempt in range(self.max_retries):
            try:
                req = Request(url, data=data, headers=headers, method=method.upper())
                with urlopen(req, timeout=self.timeout) as resp:
                    body_bytes = resp.read()
                    text = body_bytes.decode() if body_bytes else ""
                    return json.loads(text) if text else {}
            except HTTPError as e:
                try:
                    err_body = json.loads(e.read().decode())
                except Exception:
                    err_body = {}
                # 4xx (除 429) 不重试
                if 400 <= e.code < 500 and e.code != 429:
                    raise APIError(
                        err_body.get("message", f"HTTP {e.code}"),
                        status_code=e.code,
                        error_code=err_body.get("error"),
                        body=err_body,
                    )
                last_err = APIError(f"HTTP {e.code}", status_code=e.code, body=err_body)
            except URLError as e:
                last_err = APIError(f"network: {e.reason}")
            # 退避
            time.sleep(0.1 * (2 ** attempt))
        if last_err:
            raise last_err
        raise APIError("unknown")


class _ResourceAPI:
    """通用资源 — POST/GET/list 三件套."""

    def __init__(self, client: Client, name: str):
        self.c = client
        self.path = f"/v1/{name}"

    def create(self, idempotency_key: Optional[str] = None, **params) -> Dict[str, Any]:
        return self.c.request("POST", self.path, body=params, idempotency_key=idempotency_key)

    def retrieve(self, id: str) -> Dict[str, Any]:
        return self.c.request("GET", f"{self.path}/{id}")

    def list(self, **query) -> Dict[str, Any]:
        path = self.path
        if query:
            path += "?" + urlencode(query)
        return self.c.request("GET", path)


class _Webhooks:
    def __init__(self, client: Client):
        self.c = client

    def construct_event(
        self,
        raw_body: Union[bytes, str],
        signature_header: str,
        endpoint_secret: str,
        tolerance: int = 300,
    ) -> Dict[str, Any]:
        """验 webhook 签名 + 解 event.

        Args:
            raw_body: Flask request.data / Django request.body (raw bytes!)
            signature_header: X-Webhook-Signature header value
            endpoint_secret: dashboard 复制
            tolerance: 时间戳窗口秒数, 默认 300

        Returns:
            parsed event dict

        Raises:
            WebhookSignatureError
        """
        timestamp = 0
        sigs = []
        for part in signature_header.split(","):
            part = part.strip()
            if part.startswith("t="):
                try:
                    timestamp = int(part[2:])
                except ValueError:
                    raise WebhookSignatureError("bad timestamp")
            elif part.startswith("v1="):
                sigs.append(part[3:])
        if not timestamp or not sigs:
            raise WebhookSignatureError("malformed signature header")

        now = int(time.time())
        if abs(now - timestamp) > tolerance:
            raise WebhookSignatureError(f"timestamp out of tolerance (delta={now - timestamp}s)")

        body = raw_body.decode() if isinstance(raw_body, (bytes, bytearray)) else raw_body
        payload = f"{timestamp}.{body}".encode()
        expected = hmac.new(endpoint_secret.encode(), payload, hashlib.sha256).hexdigest()
        if not any(hmac.compare_digest(expected, s) for s in sigs):
            raise WebhookSignatureError("signature mismatch")
        return json.loads(body)
