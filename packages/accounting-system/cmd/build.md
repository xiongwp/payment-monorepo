cd /Users/bytedance/Documents/accounting-system
git pull --ff-only origin claude/fix-payment-util-pvFNu
go mod vendor
git add vendor && git commit -m "vendor: refresh accounting-grpc-api for RebuildHotAccounts RPC"
git push

cd /Users/bytedance/Documents/payment-admin-web/stack
docker compose build --no-cache accounting-system
docker compose up -d --force-recreate accounting-system