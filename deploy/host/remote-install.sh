#!/bin/bash
# The host side of `deploy/host/install.sh` (ADR-0006 §6 and §9): run as root on the host, from a staging
# folder install.sh copied there. Bash 3.2 (macOS's /bin/bash): no associative arrays.
#
#   remote-install.sh STAGE install [--no-service]   the user, files, sudoers, enrolment, the service
#   remote-install.sh STAGE upgrade [--no-service]   a new binary; restarts the service
#   remote-install.sh STAGE remove                   stops the service; removes all but the audit log
#
# STAGE holds flotilla-host (the binary), config.json (this host's allowlist), root.crt, agent.env
# (FLOTILLA_ADDR, FLOTILLA_SERVER_NAME, optional FLOTILLA_HEARTBEAT) and, for install only, token (the
# enrolment token, mode 600): it is deleted as soon as enrolment has used it, whatever happens.
#
# Laid out as: the binary and its config root-owned, not writable by the agent; the agent's state its
# own (mode 700); the audit log in a root-owned directory, the file the agent's but append-only (macOS
# sappnd, which only root can clear and only in single-user mode; Linux chattr +a where the kernel lets
# us); the sudoers lines `flotilla-host sudoers` writes, checked with visudo before they go in.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin

STAGE="${1:?usage: remote-install.sh STAGE install|upgrade|remove [--no-service]}"
CMD="${2:?install, upgrade or remove}"
SERVICE=1
[ "${3:-}" = --no-service ] && SERVICE=0

AGENT_USER=_flotilla
BIN=/usr/local/libexec/flotilla-host/flotilla-host
ETC=/etc/flotilla-host
STATE=/var/db/flotilla-host
LOGDIR=/var/log/flotilla-host
AUDIT=$LOGDIR/audit.jsonl
SUDOERS=/etc/sudoers.d/flotilla-host
OS="$(uname -s)"
PLIST=/Library/LaunchDaemons/com.flotilla.host.plist
UNIT=/etc/systemd/system/flotilla-host.service
LABEL=com.flotilla.host

say() { echo "  $*"; }
die() { echo "INSTALL FAIL: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run as root (sudo)"
[ -d "$STAGE" ] || die "no staging folder $STAGE"
# the token never outlives this run
trap 'rm -f "$STAGE/token"' EXIT

as_agent() {  # as_agent CMD...: run as the agent's user, with nothing of ours in its environment
  if command -v sudo >/dev/null 2>&1; then sudo -u "$AGENT_USER" -H env -i PATH=/usr/bin:/bin "$@"
  else su -s /bin/sh "$AGENT_USER" -c "$(printf '%q ' "$@")"; fi
}

make_user() {
  if id "$AGENT_USER" >/dev/null 2>&1; then say "user $AGENT_USER exists"; return; fi
  if [ "$OS" = Darwin ]; then
    # a hidden service account: a free id under 500, no shell, no home
    local uid=300
    while dscl . -list /Users UniqueID | awk '{print $2}' | grep -qx "$uid" || dscl . -list /Groups PrimaryGroupID | awk '{print $2}' | grep -qx "$uid"; do
      uid=$((uid + 1)); [ $uid -lt 400 ] || die "no free id between 300 and 399"
    done
    dscl . -create /Groups/$AGENT_USER PrimaryGroupID $uid
    dscl . -create /Users/$AGENT_USER UniqueID $uid
    dscl . -create /Users/$AGENT_USER PrimaryGroupID $uid
    dscl . -create /Users/$AGENT_USER UserShell /usr/bin/false
    dscl . -create /Users/$AGENT_USER NFSHomeDirectory /var/empty
    dscl . -create /Users/$AGENT_USER RealName "flotilla host agent"
    dscl . -create /Users/$AGENT_USER IsHidden 1
    dscl . -create /Users/$AGENT_USER Password '*'
  else
    useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$AGENT_USER"
  fi
  say "user $AGENT_USER made (no login, no shell)"
}

group_of() { id -gn "$AGENT_USER"; }

put_files() {
  install -d -m 755 -o root "$(dirname "$BIN")" "$ETC"
  install -m 755 -o root "$STAGE/flotilla-host" "$BIN.new" && mv -f "$BIN.new" "$BIN"
  install -m 644 -o root "$STAGE/config.json" "$ETC/config.json"
  install -m 644 -o root "$STAGE/root.crt" "$ETC/root.crt"
  install -m 644 -o root "$STAGE/agent.env" "$ETC/agent.env"
  install -d -m 700 -o "$AGENT_USER" -g "$(group_of)" "$STATE"
  install -d -m 755 -o root "$LOGDIR"
  if [ ! -e "$AUDIT" ]; then
    touch "$AUDIT" && chown "$AGENT_USER:$(group_of)" "$AUDIT" && chmod 600 "$AUDIT"
  fi
  append_only
  say "files in place: $BIN, $ETC (root's), $STATE ($AGENT_USER's, 700), $AUDIT"
}

append_only() {
  if [ "$OS" = Darwin ]; then
    chflags sappnd "$AUDIT" && say "audit log append-only (sappnd)"
  elif command -v chattr >/dev/null 2>&1 && chattr +a "$AUDIT" 2>/dev/null; then
    say "audit log append-only (chattr +a)"
  else
    echo "  WARN: the audit log could not be made append-only here (no chattr, or the kernel refused)"
  fi
}

put_sudoers() {
  local tmp lines
  tmp="$(mktemp)"
  "$BIN" sudoers -config "$ETC/config.json" > "$tmp"
  lines=$(grep -c "^[^#]" "$tmp" || true)  # rules, not the header comment
  if [ "$lines" = 0 ]; then
    rm -f "$tmp" "$SUDOERS"
    say "no privileged action: no sudoers file"
    return
  fi
  command -v visudo >/dev/null 2>&1 || die "visudo is missing: the sudoers lines are not checked, so they do not go in"
  visudo -cf "$tmp" >/dev/null || { rm -f "$tmp"; die "visudo refused the generated sudoers"; }
  install -d -m 755 -o root /etc/sudoers.d
  install -m 440 -o root "$tmp" "$SUDOERS"
  rm -f "$tmp"
  visudo -c >/dev/null || die "sudo's configuration as a whole does not parse after adding $SUDOERS"
  say "sudoers: $lines line(s) in $SUDOERS, checked with visudo"
}

flags() {  # the agent's flags, from agent.env (never the token)
  # shellcheck disable=SC1090
  . "$ETC/agent.env"
  [ -n "${FLOTILLA_ADDR:-}" ] && [ -n "${FLOTILLA_SERVER_NAME:-}" ] || die "agent.env needs FLOTILLA_ADDR and FLOTILLA_SERVER_NAME"
  echo "-config $ETC/config.json -state $STATE -audit $AUDIT -root $ETC/root.crt -addr $FLOTILLA_ADDR -server-name $FLOTILLA_SERVER_NAME -heartbeat ${FLOTILLA_HEARTBEAT:-30s}"
}

enrol() {
  [ -s "$STAGE/token" ] || die "no enrolment token in the staging folder"
  # shellcheck disable=SC2046
  as_agent "$BIN" enrol $(flags) < "$STAGE/token"
  rm -f "$STAGE/token"
}

service_up() {
  [ "$SERVICE" = 1 ] || { say "service: not installed (--no-service)"; return; }
  local f
  if [ "$OS" = Darwin ]; then
    {
      echo '<?xml version="1.0" encoding="UTF-8"?>'
      echo '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">'
      echo '<plist version="1.0"><dict>'
      echo "  <key>Label</key><string>$LABEL</string>"
      echo "  <key>UserName</key><string>$AGENT_USER</string>"
      echo '  <key>ProgramArguments</key><array>'
      echo "    <string>$BIN</string><string>run</string>"
      for f in $(flags); do echo "    <string>$f</string>"; done
      echo '  </array>'
      echo '  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>'
      echo "  <key>StandardErrorPath</key><string>$LOGDIR/agent.log</string>"
      echo '</dict></plist>'
    } > "$PLIST.new"
    plutil -lint "$PLIST.new" >/dev/null || die "the launchd plist does not parse"
    chown root:wheel "$PLIST.new"; chmod 644 "$PLIST.new"; mv -f "$PLIST.new" "$PLIST"
    touch "$LOGDIR/agent.log"; chown "$AGENT_USER" "$LOGDIR/agent.log"; chmod 640 "$LOGDIR/agent.log"
    launchctl bootout system/$LABEL 2>/dev/null || true
    launchctl bootstrap system "$PLIST"
    say "launchd: $LABEL loaded"
  else
    {
      echo '[Unit]'
      echo 'Description=flotilla host agent'
      echo 'After=network-online.target'
      echo '[Service]'
      echo "User=$AGENT_USER"
      echo "ExecStart=$BIN run $(flags)"
      echo 'Restart=always'
      echo 'RestartSec=5'
      echo 'NoNewPrivileges=no'
      echo '[Install]'
      echo 'WantedBy=multi-user.target'
    } > "$UNIT"
    systemctl daemon-reload && systemctl enable --now flotilla-host
    say "systemd: flotilla-host enabled and started"
  fi
}

service_down() {
  if [ "$OS" = Darwin ]; then
    launchctl bootout system/$LABEL 2>/dev/null || true
    rm -f "$PLIST"
  elif command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now flotilla-host 2>/dev/null || true
    rm -f "$UNIT"; systemctl daemon-reload 2>/dev/null || true
  fi
}

# the H3 open question: can the agent's user read a launchd job's state without root?
check_launchctl() {
  [ "$OS" = Darwin ] || return 0
  local label
  label=$(grep -o '"label": *"[^"]*"' "$ETC/config.json" | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
  [ -n "$label" ] || return 0
  if as_agent /bin/launchctl print "$label" | grep -q 'state = '; then
    say "$AGENT_USER can read $label's state (launchctl print)"
  else
    echo "  WARN: $AGENT_USER cannot read $label's state: its precondition will come back unknown"
  fi
}

case "$CMD" in
install)
  for f in flotilla-host config.json root.crt agent.env; do [ -s "$STAGE/$f" ] || die "$STAGE/$f is missing"; done
  echo "== install on $(hostname) ($OS)"
  make_user
  "$STAGE/flotilla-host" sudoers -config "$STAGE/config.json" >/dev/null || die "the config does not load"
  put_files
  put_sudoers
  enrol
  check_launchctl
  service_up
  echo "INSTALL PASS"
  ;;
upgrade)
  [ -s "$STAGE/flotilla-host" ] || die "$STAGE/flotilla-host is missing"
  echo "== upgrade on $(hostname) ($OS)"
  install -m 755 -o root "$STAGE/flotilla-host" "$BIN.new" && mv -f "$BIN.new" "$BIN"
  [ -s "$STAGE/config.json" ] && install -m 644 -o root "$STAGE/config.json" "$ETC/config.json" && put_sudoers
  if [ "$SERVICE" = 1 ]; then service_up; fi
  echo "UPGRADE PASS"
  ;;
remove)
  echo "== remove on $(hostname) ($OS)"
  service_down
  rm -f "$SUDOERS" "$BIN"; rmdir "$(dirname "$BIN")" 2>/dev/null || true
  rm -rf "$ETC" "$STATE"
  # the audit log stays: it is the record of what ran here
  say "removed; the audit log stays in $LOGDIR"
  echo "REMOVE PASS"
  ;;
*) die "unknown command $CMD" ;;
esac
