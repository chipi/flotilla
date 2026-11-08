#!/usr/bin/env bash
# Install (or remove) the state mirror as a system LaunchDaemon on the mini, running as the operator
# account every 120 s, like the homelab's other jobs (com.homelab.hub-status). Where it writes is
# MIRROR_DEST in FLOTILLA_HOME/.env (a test folder until the switch; ~/signal-fleet only after it,
# and mirror.py refuses that without MIRROR_LIVE=1, fleetd's STOP flag and a non-shadow stage).
#   install-mirror.sh            install or update, then run once and show the result
#   install-mirror.sh --remove   stop and remove it
set -euo pipefail
LABEL=com.flotilla.mirror
PLIST=/Library/LaunchDaemons/$LABEL.plist
LOG=/tmp/flotilla-mirror.log
H="${FLOTILLA_HOME:-$HOME/flotilla}"
if [ "${1:-}" = --remove ]; then
  sudo launchctl bootout system/$LABEL 2>/dev/null || true
  sudo rm -f "$PLIST"; echo "REMOVED $LABEL"; exit 0
fi
T="$(mktemp)"
cat > "$T" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>$LABEL</string>
    <key>UserName</key><string>$(id -un)</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>-lc</string>
        <string>export PATH=/usr/local/bin:/usr/bin:/bin; exec /usr/bin/python3 $H/repo/deploy/mini/mirror.py</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict><key>FLOTILLA_HOME</key><string>$H</string></dict>
    <key>StartInterval</key><integer>120</integer>
    <key>StandardOutPath</key><string>$LOG</string>
    <key>StandardErrorPath</key><string>$LOG</string>
</dict>
</plist>
EOF
plutil -lint "$T" >/dev/null
sudo launchctl bootout system/$LABEL 2>/dev/null || true
sudo install -m 644 -o root -g wheel "$T" "$PLIST"; rm -f "$T"
sudo launchctl bootstrap system "$PLIST"
sudo launchctl kickstart -k system/$LABEL
sleep 8
echo "installed $LABEL (every 120 s); last log line:"; tail -1 "$LOG"
