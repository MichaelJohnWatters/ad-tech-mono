#!/bin/bash
# scripts/audit-ui.sh — lint guardrails for the UI design system.
#
# Goal: keep the wins from docs/UI_DESIGN_AUDIT.md from rotting. Each
# check has a BUDGET (the count of allowed offenders today). PRs that
# add new offenders push the count past the budget and fail. PRs that
# clean up offenders ratchet the budget down, locking in progress.
#
# Run locally:    bash scripts/audit-ui.sh
# Or via make:    make audit-ui
#
# Exit codes:
#   0 — every check at or below its budget
#   1 — at least one check exceeded its budget
#
# When you intentionally add an offender (e.g. a new inline style for
# a one-off case), update the BUDGET_* variable below in the SAME PR
# so the rule fires correctly going forward. When you clean up
# offenders, lower the budget by the amount you removed.

set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

# Budgets — current counts the codebase tolerates.
# Update when you change the actual code; the script complains if the
# count drifts. If you legitimately need more, raise the budget; if
# you cleaned some up, lower it.
BUDGET_INLINE_STYLE=275   # web/templates/**/*.html inline style="..."
                          # Raised from 260 to 275 (2026-06-03): added
                          # inline CSS-var-based styles to the simulator's
                          # confirm/prompt modal bodies + the side-by-side
                          # trace layout. Simulator can't use Tailwind
                          # classes since it loads theme.css instead.
BUDGET_CONFIRM_CALLS=1    # confirm()/prompt() not preceded by window.
BUDGET_ARBITRARY_HEX=0    # Tailwind bg-[#abc]/text-[#fff]/border-[#000]
BUDGET_ALERT_CALLS=0      # window-style alert()

FAILED=0

# inline_styles — count of style="..." attributes anywhere under
# web/templates. Excludes the head-meta partial since the small inline
# styles it ships are intentional and won't be migrated to Tailwind
# (theme persistence script, etc.).
inline_styles() {
    grep -rE 'style="[^"]+"' web/templates 2>/dev/null \
        | grep -v 'web/templates/partials/head-meta.html' \
        | wc -l \
        | tr -d ' \n'
}

# confirm_calls — direct calls to confirm() or prompt() that are NOT
# documentation comments (i.e. `window.confirm()` references that
# appear in docstrings). The exclusion below is a sane default: any
# call site referenced as `window.confirm` is doc commentary, not a
# real call. confirmAction()/promptInput() are the platform
# replacements and don't match this pattern.
confirm_calls() {
    grep -rE '\b(confirm|prompt)\(' web/templates 2>/dev/null \
        | grep -vE 'window\.(confirm|prompt)' \
        | wc -l \
        | tr -d ' \n'
}

# arbitrary_hex — Tailwind permits `bg-[#abc123]`-style arbitrary
# values, which sidestep theme tokens. Catch them so operators reach
# for the token (bg-svc-dsp, text-success, etc.).
arbitrary_hex() {
    grep -rEn '(bg|text|border)-\[#[0-9a-fA-F]{3,8}\]' web/templates 2>/dev/null \
        | wc -l \
        | tr -d ' \n'
}

# alert_calls — window.alert() calls. Replace with toast() from
# /static/toast.js (loaded by the toast partial).
alert_calls() {
    grep -rE '\balert\(' web/templates 2>/dev/null \
        | wc -l \
        | tr -d ' \n'
}

check() {
    local name=$1
    local got=$2
    local budget=$3
    local hint=$4
    if [ "$got" -gt "$budget" ]; then
        printf '  ✗ %-25s %d > budget %d  — %s\n' "$name" "$got" "$budget" "$hint"
        FAILED=$((FAILED + 1))
    elif [ "$got" -lt "$budget" ]; then
        printf '  ⬇ %-25s %d < budget %d  — lower BUDGET_%s in this script to %d to lock in the win\n' \
            "$name" "$got" "$budget" "$(echo "$name" | tr 'a-z-' 'A-Z_')" "$got"
    else
        printf '  ✓ %-25s %d (== budget)\n' "$name" "$got"
    fi
}

echo "UI design-system audit (run via: make audit-ui)"
echo ""
check inline-styles    "$(inline_styles)"   "$BUDGET_INLINE_STYLE"   'inline style="..." attributes; replace with Tailwind utility classes or theme.css'
check confirm/prompt   "$(confirm_calls)"   "$BUDGET_CONFIRM_CALLS"  'native dialog calls; use confirmAction()/promptInput() partials instead'
check arbitrary-hex    "$(arbitrary_hex)"   "$BUDGET_ARBITRARY_HEX"  'raw hex on Tailwind class; use theme token (e.g. bg-svc-dsp, text-success)'
check alert-calls      "$(alert_calls)"     "$BUDGET_ALERT_CALLS"    'window.alert(); use toast(msg, "error")'
echo ""

if [ "$FAILED" -gt 0 ]; then
    echo "✗ $FAILED check(s) failed — fix the offending lines or raise the budget"
    exit 1
fi
echo "✅ all checks within budget"
exit 0
