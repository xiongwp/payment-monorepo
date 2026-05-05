# Monorepo root Makefile —— 验证类靶子集合。

.PHONY: verify-bindenv
verify-bindenv:
	@bash tools/verify-bindenv.sh

.PHONY: verify
verify: verify-bindenv
