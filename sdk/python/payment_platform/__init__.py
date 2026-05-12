"""Python SDK for Payment Platform.

Usage:
    from payment_platform import Client

    pay = Client(api_key="pk_test_xxx")
    charge = pay.charges.create(amount=1999, currency="USD", source="tok_xxx")
"""

from .client import Client, WebhookSignatureError, APIError

__all__ = ["Client", "WebhookSignatureError", "APIError"]
__version__ = "0.1.0"
API_VERSION = "2026-05-01"
