#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
echo "Opening Jig…"
if ! go build -o bin/jig ./cmd/jig; then
  echo "Jig could not build. Read the message above, then press Enter to close."
  read -r
  exit 1
fi
if ! ./bin/jig start; then
  echo "Jig could not start. Read the message above, then press Enter to close."
  read -r
  exit 1
fi
