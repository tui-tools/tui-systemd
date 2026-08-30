#!/bin/bash
# Backend smoke test for tui-systemd, run inside a lab guest.
#
# The contract (see tui-tools/tui-lab): this script runs on the guest as the
# unprivileged lab user, escalates with `sudo -n` only, prints a short PASS/FAIL
# table and exits non-zero if anything failed. The binary under test is at
# $TUI_LAB_BIN (default: tui-systemd on PATH).
#
# Every read in tui-systemd is unprivileged by design, so most of this runs
# without sudo at all — which is itself one of the things worth proving.
set -uo pipefail

bin="${TUI_LAB_BIN:-tui-systemd}"
# TOOL is the manifest name, which is what a compatibility result is keyed on.
TOOL=tui-systemd
pass=0
fail=0

check() {
  local label="$1" command="$2" pattern="$3" output status
  output=$(eval "$command" 2>&1)
  status=$?
  if [[ $status -eq 0 ]] && grep -qE "$pattern" <<<"$output"; then
    printf 'PASS  %s\n' "$label"
    pass=$((pass + 1))
  else
    printf 'FAIL  %s (exit %d)\n' "$label" "$status"
    sed 's/^/      | /' <<<"$output" | head -12
    fail=$((fail + 1))
  fi
}


# --- compatibility evidence -------------------------------------------------
#
# The manifest's `tested` list is generated, not claimed: it is rebuilt from
# compat/results.jsonl by tui-kit/tools/compat-sync.py, and this is where a
# line of that file comes from. The version recorded is the one the tool itself
# probed, read back out of --check, so it describes the machine that really ran
# the suite rather than what the tester assumed was installed.
#
# The line is printed behind a `compat-result:` prefix so it survives the trip
# out of the guest through the lab's per-VM log, and appended to
# $TUI_COMPAT_RESULTS as well for a run outside the lab.
record_compat() {
  local report="$1" outcome="$2" backend version distro today block line
  block=$(sed -n '/"compat": {/,/^  }/p' <<<"$report")
  backend=$(sed -n 's/.*"backend": "\([^"]*\)".*/\1/p' <<<"$block" | head -1)
  version=$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' <<<"$block" | head -1)
  if [[ -z $backend || -z $version ]]; then
    echo "      no version was probed, so no compatibility result is recorded"
    return
  fi

  distro=$(. /etc/os-release && echo "${ID}-${VERSION_ID:-rolling}")
  today=$(date -u +%Y-%m-%d)
  line=$(printf '{"backend":"%s","date":"%s","distro":"%s","result":"%s","suite":"smoke","tool":"%s","version":"%s"}' \
    "$backend" "$today" "$distro" "$outcome" "$TOOL" "$version")

  printf 'compat-result: %s\n' "$line"
  if [[ -n ${TUI_COMPAT_RESULTS:-} ]]; then
    printf '%s\n' "$line" >>"$TUI_COMPAT_RESULTS"
  fi
}

echo "--- tui-systemd smoke on $(. /etc/os-release && echo "$PRETTY_NAME")"

# The report is read once and reused: every assertion below is about the same
# snapshot of the machine, and re-reading between them would let a unit change
# state underneath the test.
report=$("$bin" --check 2>&1)
report_status=$?
if [[ $report_status -ne 0 ]]; then
  echo "FAIL  --check could not read the machine (exit $report_status)"
  sed 's/^/      | /' <<<"$report" | head -20
  exit 1
fi

# json_int pulls a top-level integer out of the report without needing jq,
# which is not installed on every one of the lab's images.
json_int() { sed -n "s/.*\"$1\": *\([0-9]*\).*/\1/p" <<<"$report" | head -1; }

check "--check reads the real systemd backend" \
  'echo "$report"' \
  '"backend": "systemd"'

# 1. Units are listed and parsed. A machine with fewer than twenty units is
#    not a booted Linux system; a parser that failed reports zero.
units=$(json_int units)
check "the unit list is parsed (units=$units)" \
  "echo $units" \
  '^[0-9]+$'
if [[ ${units:-0} -lt 20 ]]; then
  echo "FAIL  only $units units parsed, expected a booted machine to have more"
  fail=$((fail + 1))
else
  echo "PASS  $units units parsed"
  pass=$((pass + 1))
fi

# 2. The tool's count of active units agrees with systemctl's own.
#
#    Within a tolerance, not exactly: the tool read the machine and this line
#    reads it again a moment later, and a live machine moves in between —
#    Ubuntu's snapd and ModemManager churn enough to shift the count by one
#    between two consecutive reads, which was observed here. The assertion that
#    matters is "the parser agrees with systemd", and a parser that failed is
#    off by hundreds, not by one.
active=$(json_int active)
systemctl_active=$(systemctl list-units --all --plain --no-legend --no-pager \
  | awk '$3 == "active" {n++} END {print n + 0}')
drift=$(( ${active:-0} - ${systemctl_active:-0} ))
[[ $drift -lt 0 ]] && drift=$(( -drift ))
if [[ $drift -le 3 ]]; then
  echo "PASS  active count matches systemctl ($active vs $systemctl_active)"
  pass=$((pass + 1))
else
  echo "FAIL  active count is $active, systemctl says $systemctl_active (drift $drift)"
  fail=$((fail + 1))
fi

# 3. The unit-file state was merged in. Every distro here enables sshd or ssh,
#    so a zero here means list-unit-files was fetched but not joined.
enabled=$(json_int enabled)
if [[ ${enabled:-0} -gt 0 ]]; then
  echo "PASS  unit-file states merged ($enabled enabled)"
  pass=$((pass + 1))
else
  echo "FAIL  no unit reported as enabled, so list-unit-files was not merged"
  fail=$((fail + 1))
fi

# 4. The total agrees with systemctl. --check reports counts plus a five-unit
#    sample rather than every unit, so this compares totals: the tool merges
#    list-units with list-unit-files, so its total is at least the number of
#    loaded units and never fewer.
systemctl_total=$(systemctl list-units --all --plain --no-legend --no-pager | wc -l)
if [[ ${units:-0} -ge ${systemctl_total:-0} && ${systemctl_total:-0} -gt 0 ]]; then
  echo "PASS  total covers systemctl's loaded units ($units >= $systemctl_total)"
  pass=$((pass + 1))
else
  echo "FAIL  tool reports $units units, systemctl lists $systemctl_total loaded"
  fail=$((fail + 1))
fi

# 5. Timers are parsed. All three images ship at least one.
timers=$(json_int timers)
if [[ ${timers:-0} -gt 0 ]]; then
  echo "PASS  timers parsed ($timers)"
  pass=$((pass + 1))
else
  echo "FAIL  no timers parsed"
  fail=$((fail + 1))
fi

# 6. The journal read path works: --check reads one active service's log and
#    reports how much came back.
journal_bytes=$(sed -n '/"journal"/,/}/p' <<<"$report" \
  | sed -n 's/.*"bytes": *\([0-9]*\).*/\1/p' | head -1)
journal_unit=$(sed -n '/"journal"/,/}/p' <<<"$report" \
  | sed -n 's/.*"unit": *"\([^"]*\)".*/\1/p' | head -1)
if [[ ${journal_bytes:-0} -gt 0 ]]; then
  echo "PASS  journal read ($journal_unit, $journal_bytes bytes)"
  pass=$((pass + 1))
else
  echo "FAIL  journal read returned nothing for ${journal_unit:-<no unit>}"
  sed -n '/"journal"/,/}/p' <<<"$report" | sed 's/^/      | /'
  fail=$((fail + 1))
fi

# 7. Reads must not need root. This is a design property of the tool, and the
#    only way to notice it regressing is to assert it on a real machine.
check "reads work as an unprivileged user" \
  "$bin --sudo '' --check" \
  '"backend": "systemd"'

# The version systemd reported to the tool, recorded against the outcome of
# this run. $report is the same snapshot every assertion above was made on.
if [[ $fail -eq 0 ]]; then
  record_compat "$report" pass
else
  record_compat "$report" fail
fi

echo "--- tui-systemd: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
