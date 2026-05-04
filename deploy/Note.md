cd /home/user/payment-admin-web
./deploy.sh init-kms    # 首次跑
./deploy.sh up
./deploy.sh check



# 打开 http://localhost:8080/app
# 填 mch_id=demo_merchant, amount=10000, currency=PHP
# 点 创建订单 → 选 GCASH → 看 next_action



cd ~/Documents/payment-admin-web/stack
export GITHUB_TOKEN=ghp_...your_token...
./deploy.sh up