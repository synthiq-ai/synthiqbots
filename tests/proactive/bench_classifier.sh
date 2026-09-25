#!/usr/bin/env bash
# Bench the proactive yes/no/done classifier against the hand-labelled phrase
# dataset. SC-002 target: ≥95% per-class accuracy.
#
# Implementation lives in bench_classifier.py — that script mirrors the C++
# rules in src/mod-ollama-chat_proactive.cpp ClassifyPlayerMessage(). Keep the
# two in lockstep when extending either the dataset or the matching rules.
#
# Exit codes: 0 = all classes pass, 1 = at least one class below 95%, 2 = bad
# arguments / dataset missing.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "${SCRIPT_DIR}/bench_classifier.py" "$@"
