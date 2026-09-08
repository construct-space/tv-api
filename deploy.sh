#!/usr/bin/env bash
# Deploy api/tv/ to CapRover app 'tv' (tv.lisaos.dev).
# Not a git root, so we tar the directory and push via --tarFile.
set -euo pipefail

cd "$(dirname "$0")"

TAR=/tmp/tv-deploy.tar.gz

echo "→ packing $TAR"
tar -czf "$TAR" \
  --exclude='.git' \
  --exclude='construct-tv' \
  --exclude='.env' \
  --exclude='.DS_Store' \
  --exclude='deploy.sh' \
  .

echo "→ deploying to CapRover app 'tv'"
caprover deploy --appName tv --tarFile "$TAR"

echo "✓ deployed. Set env on the app: INTERNAL_SHARED_SECRET, INFERENCE_URL,"
echo "  TV_MODEL, TV_DEFAULT_CITY (Caprover → Apps → tv → App Configs)."
echo "  Health check: curl https://tv.lisaos.dev/api/health"
