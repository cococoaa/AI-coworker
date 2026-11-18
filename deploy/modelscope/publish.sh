#!/usr/bin/env bash
set -euo pipefail

: "${MODELSCOPE_API_KEY:?Set the MODELSCOPE_API_KEY GitHub Actions secret.}"
studio=https://modelscope.cn/studios/zkkwin/AI-worker
api=https://modelscope.cn/openapi/v1/studios/zkkwin/AI-worker
commit=$(git rev-parse HEAD)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cat > "$tmp/askpass" <<'EOF'
#!/bin/sh
case "$1" in
  *Username*) printf '%s\n' oauth2 ;;
  *Password*) printf '%s\n' "$MODELSCOPE_API_KEY" ;;
  *) exit 1 ;;
esac
EOF
chmod 700 "$tmp/askpass"
export GIT_ASKPASS="$tmp/askpass" GIT_TERMINAL_PROMPT=0

request() {
  local method=$1 path=$2
  shift 2
  curl --fail --silent --show-error --max-time 60 --request "$method" \
    --header "Authorization: Bearer $MODELSCOPE_API_KEY" "$@" "$api$path" |
    jq -e 'if .success then (.data // true) else error("ModelScope rejected the request") end'
}

info=$(request GET '')
if [[ $(jq -r '.sdk_type // empty' <<< "$info") != docker ]]; then
  request PATCH /settings --header 'Content-Type: application/json' \
    --data '{"sdk_type":"docker"}' >/dev/null
fi

secrets=$(request GET /secrets)
if ! jq -e '.. | strings | select(. == "REIMBURSE_UI_TOKENS")' <<< "$secrets" >/dev/null; then
  : "${REIMBURSE_UI_TOKENS:?Set REIMBURSE_UI_TOKENS in Studio secret settings or GitHub Actions secrets.}"
  jq -n '{key:"REIMBURSE_UI_TOKENS",value:env.REIMBURSE_UI_TOKENS}' |
    request POST /secrets --header 'Content-Type: application/json' --data-binary @- >/dev/null
fi

git clone --depth 1 --single-branch --branch master "$studio.git" "$tmp/studio"
git -C "$tmp/studio" rm -r --ignore-unmatch -- reimburse-agent deploy/modelscope/entrypoint.sh
git archive "$commit" Dockerfile .dockerignore reimburse-agent deploy/modelscope/entrypoint.sh |
  tar -x -C "$tmp/studio"
jq -n --arg repository https://github.com/cococoaa/AI-coworker --arg commit "$commit" \
  '{repository:$repository,branch:"main",commit:$commit}' > "$tmp/studio/deployment.json"
git -C "$tmp/studio" add -- Dockerfile .dockerignore reimburse-agent deploy/modelscope/entrypoint.sh deployment.json
if ! git -C "$tmp/studio" diff --cached --quiet; then
  git -C "$tmp/studio" -c user.name='GitHub Actions' \
    -c user.email='41898282+github-actions[bot]@users.noreply.github.com' \
    commit -m "Deploy GitHub main $commit"
  git -C "$tmp/studio" push origin HEAD:master
fi

request POST /deploy >/dev/null
for attempt in {1..60}; do
  sleep 10
  info=$(request GET '')
  status=$(jq -r '.runtime.status // .status // empty' <<< "$info")
  case "${status,,}" in
    *fail*|*error*) echo "ModelScope deploy failed: $status" >&2; exit 1 ;;
    running)
      host=$(jq -r '.host // empty' <<< "$info")
      if [[ "$host" == https://* ]] &&
        curl --fail --silent --show-error --max-time 15 "${host%/}/login" | grep -q 'REIMBURSE_UI_TOKENS'; then
        echo "Studio is running: $studio (GitHub main $commit)"
        exit 0
      fi
      ;;
  esac
done
echo "Timed out waiting for Studio: $studio" >&2
exit 1
