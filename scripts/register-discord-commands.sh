#!/usr/bin/env bash
# Registers Bellion's global Discord slash command (/status) via REST using
# client_credentials (no Gateway bot required).
#
# Usage:
#   BLITE_DISCORD_SECRET=... ./scripts/register-discord-commands.sh
set -euo pipefail

CLIENT_ID="${BLITE_DISCORD_CLIENT_ID:-1569899965323415693}"
SECRET="${BLITE_DISCORD_SECRET:?set BLITE_DISCORD_SECRET}"

# Credentials go to curl on stdin (-K -), never on its command line, where
# any local user could read them in the process list. printf is a builtin.
# cfgq escapes a value for a quoted curl config string.
cfgq() { local v=${1//\\/\\\\}; printf '%s' "${v//\"/\\\"}"; }
token_json=$(printf 'user = "%s:%s"\n' "$CLIENT_ID" "$(cfgq "$SECRET")" | curl -fsS -K - -X POST "https://discord.com/api/v10/oauth2/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials&scope=applications.commands.update")

token=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])' <<<"$token_json")

printf 'header = "Authorization: Bearer %s"\n' "$(cfgq "$token")" | curl -fsS -K - -X PUT "https://discord.com/api/v10/applications/${CLIENT_ID}/commands" \
  -H "Content-Type: application/json" \
  -d '[{"name":"status","description":"Check if Bellion servers are online","type":1}]'
echo
