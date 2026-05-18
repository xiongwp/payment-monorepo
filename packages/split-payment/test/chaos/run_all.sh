#!/usr/bin/env bash
# SP-AC-7 PH3-6: run all chaos scenarios in sequence with summary.
set -uo pipefail

cd "$(dirname "$0")"

SCENARIOS=(
  "01_accounting_timeout.sh"
  "02_accounting_drop.sh"
  # 03/04/06/07 not yet authored — templates below
  "05_split_payment_pod_kill.sh"
)

PASS=()
FAIL=()
for s in "${SCENARIOS[@]}"; do
  if [ ! -x "$s" ]; then
    chmod +x "$s"
  fi
  printf '\n========== %s ==========\n' "$s"
  if "./$s"; then
    PASS+=("$s")
  else
    FAIL+=("$s")
  fi
done

echo
echo "========== SUMMARY =========="
echo "Passed: ${#PASS[@]} / ${#SCENARIOS[@]}"
for s in "${PASS[@]}"; do echo "  ✓ $s"; done
if [ "${#FAIL[@]}" -gt 0 ]; then
  echo "Failed:"
  for s in "${FAIL[@]}"; do echo "  ✗ $s"; done
  exit 1
fi
