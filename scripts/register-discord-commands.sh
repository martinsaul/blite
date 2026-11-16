#!/usr/bin/env bash
# Registers Bellion's global Discord slash command (/status) via REST using
# client_credentials (no Gateway bot required).
#
# Usage:
#   BLITE_DISCORD_SECRET=... ./scripts/register-discord-commands.sh
set -euo pipefail

CLIENT_ID="${BLITE_DISCORD_CLIENT_ID:-1569899965323415693}"
SECRET="${BLITE_DISCORD_SECRET:?set BLITE_DISCORD_SECRET}"

token_json=$(curl -fsS -X POST "https://discord.com/api/v10/oauth2/token" \
  -u "${CLIENT_ID}:${SECRET}" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials&scope=applications.commands.update")

token=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])' <<<"$token_json")

curl -fsS -X PUT "https://discord.com/api/v10/applications/${CLIENT_ID}/commands" \
  -H "Authorization: Bearer ${token}" \
  -H "Content-Type: application/json" \
  -d '[{"name":"status","description":"Check if Bellion servers are online","type":1}]'
echo
