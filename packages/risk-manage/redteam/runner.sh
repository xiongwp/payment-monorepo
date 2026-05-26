#!/usr/bin/env bash
# runner.sh — 跑全套 6 个攻击，把实际结果跟 expected.json 比对，PASS/FAIL 输出。
#
# 用法：
#   ./runner.sh                       # 默认 TARGET_URL=http://host.docker.internal:8080/demo/checkout.html
#   TARGET_URL=http://x:8080 ./runner.sh
#   ./runner.sh 02-puppeteer-stealth  # 只跑一个
#
# 退出码：0 = 全 PASS，1 = 有 FAIL（CI 会标红）

set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

ATTACKS=(
  "01-bare-puppeteer"
  "02-puppeteer-stealth"
  "03-playwright-headed"
  "04-fake-mouse"
  "05-replay-attack"
  "06-cookie-clear-replay"
)

# 单跑模式
if [[ $# -ge 1 ]]; then
  ATTACKS=("$1")
fi

mkdir -p results
mkdir -p logs

REPORT="results/_report.txt"
: > "$REPORT"

pass_count=0
fail_count=0

# 用 jq 解析 expected.json；如果没装 jq → fallback 到 python3
have_jq=0
if command -v jq >/dev/null 2>&1; then
  have_jq=1
fi

read_expected() {
  # $1=attack_id  $2=field  → echo 值
  local id="$1" field="$2"
  if [[ $have_jq -eq 1 ]]; then
    jq -r ".\"$id\".$field // empty" expected.json
  else
    python3 -c "
import json, sys
with open('expected.json') as f:
    d = json.load(f)
v = d.get('$id', {}).get('$field')
if v is None:
    print('')
elif isinstance(v, list):
    print(' '.join(map(str, v)))
else:
    print(v)
"
  fi
}

# extract 实际结果（attack stdout 里 REDTEAM_RESULT::<json>）
extract_result() {
  local logfile="$1" field="$2"
  local line
  line="$(grep -E '^REDTEAM_RESULT::' "$logfile" | tail -1 | sed 's/^REDTEAM_RESULT:://')"
  if [[ -z "$line" ]]; then echo ""; return; fi
  if [[ $have_jq -eq 1 ]]; then
    echo "$line" | jq -r ".$field // empty"
  else
    echo "$line" | python3 -c "
import json, sys
d = json.loads(sys.stdin.read())
v = d.get('$field')
if v is None: print('')
elif isinstance(v, list): print(' '.join(map(str, v)))
else: print(v)
"
  fi
}

check_attack() {
  local id="$1"
  local script="attacks/${id}.js"
  local logfile="logs/${id}.log"

  if [[ ! -f "$script" ]]; then
    echo "[$id] SKIP — script missing: $script" | tee -a "$REPORT"
    return 2
  fi

  echo "─────────────────────────────────────────────"
  echo "[$id] running ..."
  if ! node "$script" --target="${TARGET_URL:-http://host.docker.internal:8080/demo/checkout.html}" \
       > "$logfile" 2>&1; then
    echo "[$id] node script EXITED NONZERO — see $logfile"
  fi

  local actual_verdict actual_score actual_signals
  actual_verdict="$(extract_result "$logfile" verdict)"
  actual_score="$(extract_result "$logfile" score)"
  actual_signals="$(extract_result "$logfile" signals)"

  local exp_verdict exp_verdict_in exp_score_min exp_sig_present exp_sig_absent
  exp_verdict="$(read_expected "$id" expected_verdict)"
  exp_verdict_in="$(read_expected "$id" 'expected_verdict_in | join(\" \")')"
  exp_score_min="$(read_expected "$id" expected_score_min)"
  exp_sig_present="$(read_expected "$id" 'expected_signals_present | join(\" \")')"
  exp_sig_absent="$(read_expected "$id" 'expected_signals_absent | join(\" \")')"

  local status="PASS"
  local reason=""

  # 1) verdict
  if [[ -n "$exp_verdict" ]] && [[ "$actual_verdict" != "$exp_verdict" ]]; then
    status="FAIL"
    reason+="verdict=$actual_verdict want $exp_verdict; "
  fi
  if [[ -n "$exp_verdict_in" ]]; then
    if ! echo " $exp_verdict_in " | grep -q " $actual_verdict "; then
      status="FAIL"
      reason+="verdict=$actual_verdict not in [$exp_verdict_in]; "
    fi
  fi

  # 2) score min
  if [[ -n "$exp_score_min" ]] && [[ -n "$actual_score" ]]; then
    # bash 不能直接 float 比较；用 awk
    if awk "BEGIN{exit !($actual_score < $exp_score_min)}"; then
      status="FAIL"
      reason+="score=$actual_score < min $exp_score_min; "
    fi
  fi

  # 3) signals present
  for sig in $exp_sig_present; do
    if ! echo " $actual_signals " | grep -q " $sig "; then
      status="FAIL"
      reason+="missing signal $sig; "
    fi
  done

  # 4) signals absent
  for sig in $exp_sig_absent; do
    if echo " $actual_signals " | grep -q " $sig "; then
      status="FAIL"
      reason+="unexpected signal $sig present; "
    fi
  done

  if [[ "$status" == "PASS" ]]; then
    echo "[$id] PASS verdict=$actual_verdict score=$actual_score" | tee -a "$REPORT"
    pass_count=$((pass_count+1))
    return 0
  else
    echo "[$id] FAIL — $reason" | tee -a "$REPORT"
    echo "        (log: $logfile)" | tee -a "$REPORT"
    fail_count=$((fail_count+1))
    return 1
  fi
}

echo "Red Team runner — target=${TARGET_URL:-http://host.docker.internal:8080/demo/checkout.html}"
echo "Attacks: ${ATTACKS[*]}"
echo

for id in "${ATTACKS[@]}"; do
  check_attack "$id" || true
done

echo
echo "─────────────────────────────────────────────"
echo "Summary: PASS=$pass_count  FAIL=$fail_count  (out of ${#ATTACKS[@]})"
echo "Detailed report: $REPORT"

if [[ $fail_count -gt 0 ]]; then
  exit 1
fi
exit 0
