#!/bin/sh
# setup-pi.sh - Mira-Thing Raspberry Pi provisioning wizard (epic 10)
#
# Runs ON THE DEVICE (armv7l, POSIX sh / busybox compatible) and provisions
# the USB-Ethernet-connected Raspberry Pi compute server:
#   1. connection test against the Pi's SSH port
#   2. model auto-detection via /proc/device-tree/model
#   3. deploy (both tiers run the SAME nodejs compute-server on :8080,
#      serving the epic 10 T2 route contract with CORS
#      Access-Control-Allow-Origin: * because the UI origin
#      http://localhost:80 is cross-origin to the Pi):
#        GET /api/v1/capabilities -> {"tier":"cache"|"compute",
#                                     "disk_cache":bool,
#                                     "remote_colors":bool,
#                                     "remote_blur":bool}
#        GET /img/<urlencoded-cdn-url>/160.jpg -> artwork (real 160x160
#                              JPEG resize via jpeg-js; non-JPEG/PNG sources
#                              or a missing jpeg-js install -> the original
#                              bytes are passed through unchanged)
#        GET /img/<urlencoded-cdn-url>/colors  -> {"dominant":[r,g,b]}
#                              (4-bit quant histogram; decode failure ->
#                              grey [128,128,128])
#      Disk cache: /var/cache/mira/img/<sha1(url)>/{160.jpg,colors.json}.
#
#      Design decision (epic 10 follow-up): the Pi Zero W (lightweight)
#      tier no longer gets the old nginx static tier. A static file server
#      has no fetcher, so its /img/ disk cache could never be filled and
#      the advertised remote_colors/remote_blur features could never be
#      delivered. jpeg-js is pure JS (no native build step), which makes
#      the node service viable on armv6/armv7 as well. The tier only
#      changes the capabilities tier field (cache vs compute) and the
#      default cache cap (200 vs 500 files) inside the service.
#
# Environment (set by the daemon's /api/setup-pi, or manually when run by hand):
#   SSH_HOST  Pi IP address (default network: 192.168.7.1)
#   SSH_USER  ssh user on the Pi
#   SSH_PASS  ssh password (never printed; handed to sshpass via SSHPASS)
#   MIRA_SSH_KEY_PATH  path of the device ssh key for the key-first attempt
#                      (epic 10 ticket10-3; default /etc/mira/ssh/id_ed25519)
#   MIRA_COMPUTE_JS  path of compute-server.js (default: next to this
#                    script; override for tests)
#
# On success the script prints the machine-readable line
#   RESULT model="<model>" tier="<lightweight|compute>"
# and exits 0. Any failure exits non-zero with an ERROR line on stderr.

set -u

SSH_HOST="${SSH_HOST:-}"
SSH_USER="${SSH_USER:-}"
SSH_PASS="${SSH_PASS:-}"

log() {
    printf '[setup-pi] %s\n' "$*"
}

die() {
    printf '[setup-pi] ERROR: %s\n' "$*" >&2
    exit 1
}

[ -n "$SSH_HOST" ] || die "SSH_HOST env var is required"
[ -n "$SSH_USER" ] || die "SSH_USER env var is required"
[ -n "$SSH_PASS" ] || die "SSH_PASS env var is required"

command -v sshpass >/dev/null 2>&1 || die "sshpass is not installed on this device (firmware image from before epic 10?)"
command -v ssh >/dev/null 2>&1 || die "ssh client is not installed on this device"

# sshpass -e reads the password from the SSHPASS env var so it never shows
# up in the process list (sshpass -p would)
export SSHPASS="$SSH_PASS"

# epic 10 ticket10-3: key-first SSH. The daemon generates the device key
# pair lazily at this path before exec'ing this script (it passes the path
# as MIRA_SSH_KEY_PATH; the default is the rootfs location created by the
# firmware build). Once a finished run installed the key on the Pi, every
# operation can run without the password.
MIRA_SSH_KEY_PATH="${MIRA_SSH_KEY_PATH:-/etc/mira/ssh/id_ed25519}"

# key attempt: BatchMode (no password prompt, fails fast), no inherited
# agent (SSH_AUTH_SOCK emptied, so a foreign key cannot mask whether OUR
# key works), accept-new known-host handling
run_ssh_key() {
    SSH_AUTH_SOCK= ssh -i "$MIRA_SSH_KEY_PATH" \
        -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
        -o ConnectTimeout=10 -o LogLevel=ERROR "$SSH_USER@$SSH_HOST" "$1"
}

run_ssh_pass() {
    sshpass -e ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=10 -o LogLevel=ERROR "$SSH_USER@$SSH_HOST" "$1"
}

# run one command (or a multi-line script) on the Pi: key-first, password
# fallback. A key attempt that fails with ssh exit 255 is a connection or
# authentication problem -> retry with the password. Any other exit code is
# the REMOTE command's own exit code (it already ran) -> returned as-is, a
# password retry would double-execute it. Without the key file (manual runs
# on older images) the behaviour is unchanged: straight to sshpass.
run_ssh() {
    if [ -f "$MIRA_SSH_KEY_PATH" ]; then
        run_ssh_key "$1"
        rc=$?
        if [ "$rc" -ne 255 ]; then
            return "$rc"
        fi
        # rc 255: connection/authentication level - fall through
    fi
    run_ssh_pass "$1"
}

log "provisioning $SSH_HOST (user: $SSH_USER)"

# ---------------------------------------------------------------- step 1:
# connection test against the Pi's SSH port
log "step 1/3: connection test on $SSH_HOST:22"
if command -v nc >/dev/null 2>&1; then
    nc -z -w 5 "$SSH_HOST" 22 >/dev/null 2>&1 || die "cannot reach $SSH_HOST:22 (port closed or unreachable)"
else
    probe="$(ssh -o ConnectTimeout=5 -o BatchMode=yes -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$SSH_USER@$SSH_HOST" true 2>&1)" || true
    case "$probe" in
        *"Connection refused"*|*"Connection timed out"*|*"Could not resolve"*|*"No route to host"*)
            die "cannot reach $SSH_HOST:22 (port closed or unreachable)" ;;
    esac
fi
log "ssh port reachable"

# ---------------------------------------------------------------- step 2:
# model detection. /proc/device-tree/model is NUL-terminated; [:print:] drops
# the NUL. head -c caps the read.
log "step 2/3: detecting Pi model"
MODEL="$(run_ssh "head -c 64 /proc/device-tree/model 2>/dev/null | tr -cd '[:print:]'")" || die "ssh model detection failed"
[ -n "$MODEL" ] || die "could not detect Pi model (/proc/device-tree/model is empty)"
log "detected model: $MODEL"

case "$MODEL" in
    *"Pi Zero 2 W"*)
        TIER="compute"
        ;;
    *"Pi Zero W"*)
        TIER="lightweight"
        ;;
    *"Pi 4 Model"*|*"Pi 400"*)
        TIER="compute"
        ;;
    *)
        TIER="lightweight"
        log "WARNING: unrecognised model '$MODEL' - deploying lightweight tier as safe default"
        ;;
esac
log "selected tier: $TIER"

# ---------------------------------------------------------------- step 3:
# NOTE: package-manager detection happens REMOTELY in each deploy block,
# because the Pi (Debian/Raspbian/Alpine/Void) and the device (Void) have
# different package managers.
#
# Both tiers deploy the same node compute service (epic 10 follow-up):
# the tier only selects MIRA_PI_TIER (capabilities tier field + default
# cache cap inside the service). The remote programs are single-quoted
# blocks (no local expansion, no single quotes inside) or double-quoted
# lines where the local variables are the intended expansion.
log "step 3/3: deploying node compute service on $SSH_HOST (tier: $TIER)"

# 3a: install node if needed (per-distro package manager, remotely)
run_ssh '
set -e
if command -v node >/dev/null 2>&1; then
    echo "node already installed"
else
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends nodejs
    elif command -v apk >/dev/null 2>&1; then
        apk add --no-progress nodejs
    elif command -v xbps-install >/dev/null 2>&1; then
        xbps-install -y nodejs
    else
        echo "no supported package manager found" >&2
        exit 1
    fi
fi
' || die "node install failed"

# 3b: remove the legacy lightweight tier (nginx on :8080) if a previous
# run installed it, so :8080 is free for the node service
run_ssh '
if [ -f /etc/nginx/conf.d/mira.conf ]; then
    rm -f /etc/nginx/conf.d/mira.conf
    if command -v nginx >/dev/null 2>&1; then
        nginx -s stop 2>/dev/null || service nginx stop 2>/dev/null || sv stop nginx 2>/dev/null || rc-service nginx stop 2>/dev/null || true
        echo "removed legacy nginx tier (conf removed, nginx stopped)"
    fi
fi
' || die "legacy tier cleanup failed"

# 3c: write the service file. The JS is read from disk (MIRA_COMPUTE_JS
# override, default: next to this script) and transferred base64 encoded
# so no shell quoting can mangle it.
COMPUTE_JS_SRC="${MIRA_COMPUTE_JS:-$(dirname "$0")/compute-server.js}"
[ -f "$COMPUTE_JS_SRC" ] || die "compute-server.js not found at $COMPUTE_JS_SRC (set MIRA_COMPUTE_JS to override)"
B64="$(cat "$COMPUTE_JS_SRC" | base64 | tr -d '\n')" || die "base64 encoding failed"
run_ssh "mkdir -p /opt/mira /var/log && printf %s \"$B64\" | base64 -d > /opt/mira/compute-server.js" \
    || die "compute deploy: service file write failed"

# 3d: npm + jpeg-js, best-effort. npm is missing on some distros (nodejs
# without npm), and a failed install must NOT abort the provisioning: the
# service degrades to 160.jpg passthrough + grey colors and the UI
# fallbacks apply (documented degradation, see compute-server.js header).
run_ssh '
if command -v npm >/dev/null 2>&1; then
    echo "npm present"
else
    echo "npm missing, attempting install"
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends npm
    elif command -v apk >/dev/null 2>&1; then
        apk add --no-progress npm
    elif command -v xbps-install >/dev/null 2>&1; then
        xbps-install -y npm
    fi
fi
if command -v npm >/dev/null 2>&1; then
    npm install --prefix /opt/mira jpeg-js --no-audit --no-fund --loglevel=error || echo "WARNING: npm install jpeg-js failed - degraded mode (160.jpg passthrough + grey colors) until jpeg-js is installed"
else
    echo "WARNING: npm unavailable on pi - degraded mode (160.jpg passthrough + grey colors); install npm manually and re-run setup"
fi
' || die "npm / jpeg-js step failed"

# 3e: start (or restart) the service. NEVER pkill -f: the pattern would
# match this ssh session's own command line. Restart by pid file. The
# tier env feeds the capabilities tier field + the default cache cap.
run_ssh "export MIRA_PI_MODEL=\"$MODEL\" MIRA_PI_TIER=\"$TIER\"; if [ -f /opt/mira/compute-server.pid ]; then kill \"\$(cat /opt/mira/compute-server.pid)\" 2>/dev/null || true; sleep 1; fi; nohup node /opt/mira/compute-server.js > /var/log/mira-compute.log 2>&1 < /dev/null & echo \$! > /opt/mira/compute-server.pid" \
    || die "compute deploy: service start failed"

# 3f: health check: process alive + endpoint answers
if run_ssh "sleep 2; if kill -0 \"\$(cat /opt/mira/compute-server.pid)\" 2>/dev/null; then if command -v curl >/dev/null 2>&1; then curl -sf http://127.0.0.1:8080/api/v1/capabilities; echo; else echo \"ok (no curl)\"; fi; else echo 'compute server not running' >&2; tail -5 /var/log/mira-compute.log 2>/dev/null; exit 1; fi"; then
    log "compute endpoint check passed"
else
    die "compute endpoint check failed"
fi

log "provisioning finished: model '$MODEL' -> tier '$TIER'"
# machine-readable result line, parsed by the daemon's /api/setup-pi/status
printf 'RESULT model="%s" tier="%s"\n' "$MODEL" "$TIER"
exit 0
