rm -f /Users/bytedance/source/payment-monorepo/.git/index.lock

cd /Users/bytedance/source/payment-monorepo
rm -f 'packages/risk-manage/[builder' packages/risk-manage/ERROR
git add -A
git commit -m "chore: remove stray empty files in risk-manage"

git checkout main
git merge --ff-only feat/shadow-traffic
git push origin main
