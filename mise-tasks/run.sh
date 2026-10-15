#!/usr/bin/env bash
#MISE description="Run using process-compose"
#MISE alias="r"
#MISE interactive=true

set -euo pipefail

: "${MISE_PROJECT_ROOT:=${BASH_SOURCE[0]%/*}/..}"
export MISE_PROJECT_ROOT

[[ -d "${MISE_PROJECT_ROOT}/dist" ]] || mkdir -v "${MISE_PROJECT_ROOT}/dist"

process-compose --port 18099 -f "${MISE_PROJECT_ROOT}/mise-tasks/process-compose.yaml"
