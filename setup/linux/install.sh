#!/bin/sh
# Installs tor (Tor Expert Bundle) as a systemd service plus torbridge with a timer.
#
# tor-expert-bundle-linux-x86_64-*.tar.gz and torbridge must be next to this script.
# No internet is needed for the installation itself; bridges are selected right after it.
#
#   sudo ./install.sh                install or upgrade (an existing torrc is kept)
#   sudo ./install.sh --reset-torrc  also rewrite the base torrc
#   sudo ./install.sh --uninstall    remove the service, the timers and /opt/torbridge
#
# Exit codes: 0 installed and connected, 3 installed but no working bridges yet, 1 error.
set -eu

PREFIX=/opt/torbridge
USER_NAME=torbridge
UNITS=/etc/systemd/system
HERE=$(cd "$(dirname "$0")" && pwd)

# Exit relays are restricted to these countries; edit and re-run with --reset-torrc to change.
EXIT_NODES='{de},{nl},{ch},{at},{se},{no},{fi},{dk},{is},{fr},{be},{lu},{ie},{gb},{ee},{lt},{lv},{pl},{cz},{ca},{us}'

RESET_TORRC=0
UNINSTALL=0
for a in "$@"; do
    case $a in
        --reset-torrc) RESET_TORRC=1 ;;
        --uninstall) UNINSTALL=1 ;;
        *) echo "unknown option: $a" >&2; exit 1 ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    exec sudo "$0" "$@"
fi

step() { printf '\033[36m[%s/6] %s\033[0m\n' "$1" "$2"; }
die() { printf '\n\033[31mERROR: %s\033[0m\n' "$*"; exit 1; }

# Final banner; waits for Enter when run from a terminal so the result can be read.
finish() {
    rc=$?
    line='================================================================'
    printf '\n%s\n' "$line"
    case $rc in
        0) if [ "$UNINSTALL" -eq 1 ]; then echo ' SUCCESS: TorBridge removed.'; else
               echo ' SUCCESS: Tor is installed and connected.'
               echo " SOCKS proxy ${SOCKS:-?}, HTTP proxy ${HTTP:-?}"
               echo ' Bridges refresh weekly; force now: sudo torbridge update'; fi ;;
        3) echo ' INSTALLED, but the Tor connection is not up yet.'
           echo ' torbridge-auto.timer will retry; to retry now: sudo torbridge update' ;;
        *) echo " FAILED (code $rc). See the messages above."
           echo " Log: $PREFIX/logs/torbridge.log" ;;
    esac
    printf '%s\n' "$line"
    if [ -t 0 ] && [ -t 1 ]; then
        printf 'Press Enter to close...'
        read -r _ || true
    fi
}

port_busy() {
    # Something already listens on this port (any address).
    command -v ss >/dev/null || return 1
    ss -Hltn "( sport = :$1 )" 2>/dev/null | grep -q .
}

remove_all() {
    for u in torbridge-auto.timer torbridge-stats.timer torbridge-tor.service; do # stats: 1.3 only
        systemctl disable --now "$u" >/dev/null 2>&1 || true
    done
    rm -f "$UNITS"/torbridge-tor.service "$UNITS"/torbridge-auto.service "$UNITS"/torbridge-auto.timer \
        "$UNITS"/torbridge-update.service "$UNITS"/torbridge-stats.service "$UNITS"/torbridge-stats.timer
    systemctl daemon-reload
    rm -f /usr/local/bin/torbridge /usr/local/bin/bridgestat
}

trap finish EXIT

if [ "$UNINSTALL" -eq 1 ]; then
    echo "Removing the tor service, timers and $PREFIX..."
    remove_all
    rm -rf "$PREFIX"
    userdel "$USER_NAME" >/dev/null 2>&1 || true
    exit 0
fi

step 1 "Checking installation files"
[ "$(uname -m)" = x86_64 ] || die "only x86_64 is supported (this is $(uname -m))"
command -v systemctl >/dev/null || die "systemd is required"
BUNDLE=$(ls "$HERE"/tor-expert-bundle-linux-x86_64-*.tar.gz 2>/dev/null | sort -V | tail -n 1)
[ -n "$BUNDLE" ] || die "tor-expert-bundle-linux-x86_64-*.tar.gz not found next to the script"
[ -f "$HERE/torbridge" ] || die "torbridge not found next to the script"
echo "      $(basename "$BUNDLE"), torbridge"

step 2 "Unpacking Tor to $PREFIX"
systemctl stop torbridge-tor.service >/dev/null 2>&1 || true
mkdir -p "$PREFIX/state" "$PREFIX/logs"
tar -xzf "$BUNDLE" -C "$PREFIX" --exclude='debug' || die "cannot unpack $(basename "$BUNDLE")"
install -m 755 "$HERE/torbridge" "$PREFIX/"
ln -sf "$PREFIX/torbridge" /usr/local/bin/torbridge
# 1.3 had a separate bridgestat tool, timer and history file; keep the collected history.
rm -f "$PREFIX/bridgestat" /usr/local/bin/bridgestat
[ -f "$PREFIX/logs/bridgestat.jsonl" ] && [ ! -f "$PREFIX/logs/history.jsonl" ] && mv "$PREFIX/logs/bridgestat.jsonl" "$PREFIX/logs/history.jsonl"
systemctl disable --now torbridge-stats.timer >/dev/null 2>&1 || true
rm -f "$UNITS/torbridge-stats.service" "$UNITS/torbridge-stats.timer"
VERSION=$(LD_LIBRARY_PATH="$PREFIX/tor" "$PREFIX/tor/tor" --version 2>&1 | head -n 1) || die "tor does not run: $VERSION"
echo "      $VERSION"

step 3 "Writing configuration"
id "$USER_NAME" >/dev/null 2>&1 || useradd --system --no-create-home --home-dir "$PREFIX/state" \
    --shell "$(command -v nologin || echo /bin/false)" "$USER_NAME"
[ -f "$PREFIX/torrc-defaults" ] || : > "$PREFIX/torrc-defaults"
TORRC="$PREFIX/torrc"
if [ "$RESET_TORRC" -eq 1 ] || [ ! -f "$TORRC" ]; then
    # Another tor may already use the standard ports; pick a free set then.
    SOCKS=9050 CONTROL=9051 HTTP=9080
    if port_busy 9050 || port_busy 9051 || port_busy 9080; then
        SOCKS=9150 CONTROL=9151 HTTP=9180
        if port_busy 9150 || port_busy 9151 || port_busy 9180; then
            SOCKS=9250 CONTROL=9251 HTTP=9280
        fi
        echo "      standard ports are busy, using $SOCKS/$CONTROL/$HTTP"
    fi
    cat > "$TORRC" <<EOF
# Base tor config (install.sh). Bridges live in bridges.conf, written by torbridge.
DataDirectory $PREFIX/state
GeoIPFile $PREFIX/data/geoip
GeoIPv6File $PREFIX/data/geoip6
SocksPort 127.0.0.1:$SOCKS
HTTPTunnelPort 127.0.0.1:$HTTP
ControlPort 127.0.0.1:$CONTROL
CookieAuthentication 1
Log notice file $PREFIX/logs/tor.log
AvoidDiskWrites 1
ExitNodes $EXIT_NODES
StrictNodes 1
%include $PREFIX/bridges.conf
EOF
    echo "      $TORRC written"
else
    echo "      $TORRC kept (use --reset-torrc to rewrite)"
fi
[ -f "$PREFIX/bridges.conf" ] || printf '# no bridges yet: run torbridge update\nUseBridges 1\n' > "$PREFIX/bridges.conf"
chown -R root:root "$PREFIX"
# The Tor archive has 0700 directories; the service user must be able to run tor and read data.
chmod -R u=rwX,go=rX "$PREFIX"
chown -R "$USER_NAME": "$PREFIX/state" "$PREFIX/logs"
chmod 700 "$PREFIX/state"

step 4 "Registering the tor service"
cat > "$UNITS/torbridge-tor.service" <<EOF
[Unit]
Description=Tor client managed by torbridge
After=network-online.target
Wants=network-online.target

[Service]
User=$USER_NAME
Environment=LD_LIBRARY_PATH=$PREFIX/tor
ExecStart=$PREFIX/tor/tor -f $TORRC --defaults-torrc $PREFIX/torrc-defaults
Restart=on-failure
RestartSec=60
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable torbridge-tor.service >/dev/null 2>&1
echo '      torbridge-tor.service: enabled, restart on failure'

step 5 "Registering timers"
oneshot() { # name, description, arguments
    cat > "$UNITS/$1.service" <<EOF
[Unit]
Description=$2
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=$PREFIX/$3
TimeoutStartSec=2h
EOF
}
oneshot torbridge-auto "torbridge: refresh bridges if they are older than a week or the connection is down" "torbridge auto -root $PREFIX"
oneshot torbridge-update "torbridge: find new bridges now" "torbridge update -root $PREFIX"
cat > "$UNITS/torbridge-auto.timer" <<EOF
[Unit]
Description=torbridge daily check

[Timer]
OnCalendar=*-*-* 10:00:00
OnBootSec=10min
Persistent=true

[Install]
WantedBy=timers.target
EOF
systemctl daemon-reload
systemctl enable --now torbridge-auto.timer >/dev/null 2>&1
echo '      torbridge-auto.timer: daily at 10:00 and 10 min after boot'

step 6 "Finding working bridges (this takes a few minutes)"
set +e
"$PREFIX/torbridge" update -root "$PREFIX"
RC=$?
set -e
SOCKS=$(awk '$1=="SocksPort"{print $2; exit}' "$TORRC")
HTTP=$(awk '$1=="HTTPTunnelPort"{print $2; exit}' "$TORRC")
[ "$RC" -eq 0 ] || exit 3
exit 0
