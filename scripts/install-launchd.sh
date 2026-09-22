#!/bin/bash
# install-launchd.sh — install AegisGo serve as a macOS launchd service.
#
# Why launchd: the bot must survive reboots and crashes, and exactly ONE
# instance may poll getUpdates (Telegram 409s on the second). launchd gives
# us auto-restart + KeepAlive + a single definition; the token stays OUT of
# the repo (loaded from ~/.hermes/data/aegisgo-growth/aegisgo.env, 0600).
#
# Usage:  bash scripts/install-launchd.sh   (from repo root, then it just works)
# Remove: launchctl bootout gui/$(id -u)/com.aegisgo.serve

set -euo pipefail

LABEL="com.aegisgo.serve"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
ENVFILE="$HOME/.hermes/data/aegisgo-growth/aegisgo.env"
BIN="$HOME/.hermes/bin/aegis-serve"

# 1) Binary in a stable location (repo checkouts move; bin/ is gitignored).
mkdir -p "$HOME/.hermes/bin"
go build -o "$BIN" ./cmd/aegis-serve

# 2) Env file must exist and be private.
if [[ ! -f "$ENVFILE" ]]; then
  echo "missing $ENVFILE — create it (AEGIS_TELEGRAM_TOKEN=... etc.)" >&2
  exit 1
fi
chmod 600 "$ENVFILE"

# 3) Boot out any previous version, stop any manually-started instances
#    (two pollers = Telegram 409; see ADR/handoff 2026-09-22).
launchctl bootout "gui/$(id -u)/${LABEL}" 2>/dev/null || true
pkill -f "aegis-serve" 2>/dev/null || true
# Long-poll connections linger server-side ~TTL; give them time to die
# before the new instance starts polling.
sleep 65

# 4) Write the plist.
mkdir -p "$(dirname "$PLIST")"
cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${BIN}</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>AEGIS_DB_PATH</key>
    <string>$(: grep '^AEGIS_DB_PATH=' "$ENVFILE" | cut -d= -f2-)</string>
    <key>AEGIS_TELEGRAM_TOKEN</key>
    <string>$(: grep '^AEGIS_TELEGRAM_TOKEN=' "$ENVFILE" | cut -d= -f2-)</string>
    <key>AEGIS_TELEGRAM_CHATS</key>
    <string>$(: grep '^AEGIS_TELEGRAM_CHATS=' "$ENVFILE" | cut -d= -f2-)</string>
    <key>AEGIS_TELEGRAM_MODE</key>
    <string>$(: grep '^AEGIS_TELEGRAM_MODE=' "$ENVFILE" | cut -d= -f2-)</string>
    <key>AEGIS_LLM</key>
    <string>$(: grep '^AEGIS_LLM=' "$ENVFILE" | cut -d= -f2-)</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key><false/>
    <key>Crashed</key><true/>
    <key>ThrottleInterval</key>
    <integer>30</integer>
  </dict>
  <key>StandardOutPath</key><string>$HOME/.hermes/data/aegisgo-growth/serve.log</string>
  <key>StandardErrorPath</key><string>$HOME/.hermes/data/aegisgo-growth/serve.log</string>
</dict>
</plist>
EOF

# 5) Load it.
launchctl bootstrap "gui/$(id -u)" "$PLIST"
sleep 3
if pgrep -f "$BIN" >/dev/null; then
  echo "OK: ${LABEL} running ($(pgrep -f "$BIN" | head -1))"
  echo "log: tail -f $HOME/.hermes/data/aegisgo-growth/serve.log"
else
  echo "service did not start — check:" >&2
  launchctl print "gui/$(id -u)/${LABEL}" 2>&1 | tail -20 >&2
  exit 1
fi
