from setuptools import setup, find_packages

setup(
    name="payment-platform-sdk",
    version="0.1.0",
    description="Official Python SDK for Payment Platform API",
    packages=find_packages(),
    python_requires=">=3.8",
    license="Apache-2.0",
    install_requires=[],  # 只用 stdlib (urllib + hashlib + hmac), 零依赖
)
