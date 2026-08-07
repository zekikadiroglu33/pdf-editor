#!/usr/bin/env bash
# Run this on the server (/opt/pdfer) to deploy the latest master.
set -euo pipefail

cd "$(dirname "$0")"

git fetch origin master
git checkout master
git reset --hard origin/master
docker compose up -d --build
docker image prune -f
