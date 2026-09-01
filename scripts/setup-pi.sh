#!/bin/sh
# setup-pi.sh - Mira-Thing Raspberry Pi provisioning wizard (epic 10)
#
# Runs ON THE DEVICE (armv7l, POSIX sh / busybox compatible) and provisions
# the USB-Ethernet-connected Raspberry Pi compute server:
#   1. connection test against the Pi's SSH port
#   2. model auto-detection via /proc/device-tree/model
#   3. tier deploy (both tiers serve the epic 10 T2 route contract on :8080
#      with CORS Access-Control-Allow-Origin: * because the UI origin
#      http://localhost:80 is cross-origin to the Pi):
#        GET /api/v1/capabilities -> {"tier":"cache"|"compute",
#                                     "disk_cache":bool,
#                                     "remote_colors":bool,
#                                     "remote_blur":bool}
#        GET /img/<urlencoded-cdn-url>/160.jpg -> artwork
#        GET /img/<urlencoded-cdn-url>/colors  -> {"dominant":[r,g,b]}
#      Tier services:
#        Pi Zero W          -> lightweight: nginx :8080 (capabilities + /img/
#                              disk-cache alias) + SQLite cache file
#        Pi Zero 2 W / Pi 4 -> compute: nodejs compute-server on :8080
#                              (capabilities + /img/ pass-through; Sharp/Canvas
#                              image preprocessing is a later epic 10 task)
#
# Environment (set by the daemon's /api/setup-pi, or manually when run by hand):
#   SSH_HOST  Pi IP address (default network: 192.168.7.1)
#   SSH_USER  ssh user on the Pi
#   SSH_PASS  ssh password (never printed; handed to sshpass via SSHPASS)
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

# run one command (or a multi-line script) on the Pi
run_ssh() {
    sshpass -e ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=10 -o LogLevel=ERROR "$SSH_USER@$SSH_HOST" "$1"
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
log "step 3/3: deploying $TIER tier on $SSH_HOST"

if [ "$TIER" = "lightweight" ]; then
    # nginx :8080 serving the capabilities JSON + the /img/ disk-cache dir,
    # plus the SQLite cache db (content addressing lives in the daemon/UI).
    # The whole remote program is one single-quoted block: no local
    # expansion, no single quotes inside.
    run_ssh '
set -e
if command -v nginx >/dev/null 2>&1; then
    echo "nginx already installed"
else
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends nginx
    elif command -v apk >/dev/null 2>&1; then
        apk add --no-progress nginx
    elif command -v xbps-install >/dev/null 2>&1; then
        xbps-install -y nginx
    else
        echo "no supported package manager found" >&2
        exit 1
    fi
fi
mkdir -p /etc/nginx/conf.d /var/cache/mira/img /var/lib/mira
cat > /etc/nginx/conf.d/mira.conf << MIRAEOF
server {
    listen 8080;
    server_name _;
    # UI origin http://localhost:80 is cross-origin to the Pi (epic 10 T2)
    add_header Access-Control-Allow-Origin * always;
    location = /api/v1/capabilities {
        default_type application/json;
        return 200 "{\"tier\":\"cache\",\"disk_cache\":true,\"remote_colors\":false,\"remote_blur\":false}";
    }
    # /img/<urlencoded-cdn-url>/160.jpg -> /var/cache/mira/img/<decoded-url>/160.jpg
    location /img/ {
        alias /var/cache/mira/img/;
    }
}
MIRAEOF
nginx -t >/dev/null 2>&1 || { echo "nginx config test failed" >&2; exit 1; }
if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 /var/lib/mira/cache.db "CREATE TABLE IF NOT EXISTS img (uri TEXT PRIMARY KEY, path TEXT, ts INTEGER);"
else
    [ -f /var/lib/mira/cache.db ] || : > /var/lib/mira/cache.db
fi
nginx -s reload 2>/dev/null || nginx
sleep 1
' || die "lightweight deploy failed"
    if run_ssh "command -v curl >/dev/null 2>&1" >/dev/null 2>&1; then
        caps="$(run_ssh "curl -sf http://127.0.0.1:8080/api/v1/capabilities")" || die "lightweight endpoint check failed"
        log "endpoint check: $caps"
    else
        log "WARNING: no curl on the Pi, skipping endpoint check"
    fi
else
    # compute tier: nodejs service on :8080. The JS is transferred base64
    # encoded so no shell quoting can mangle it.
    COMPUTE_JS='
const http = require("http");
const os = require("os");

const PORT = 8080;
// epic 10 T2 contract: {"tier":..., "disk_cache":..., "remote_colors":..., "remote_blur":...}
const CAPS = JSON.stringify({
  tier: "compute",
  disk_cache: true,
  remote_colors: true,
  remote_blur: true,
  host: os.hostname(),
  model: process.env.MIRA_PI_MODEL || ""
});

// /img/<urlencoded-cdn-url>/<160.jpg|colors> (epic 10 T2 contract); the
// encoded URL contains no raw slashes, so the path splits cleanly
const IMG_RE = /^\/img\/([^/]+)\/([^/]+)$/;

http
  .createServer((req, res) => {
    // the UI (http://localhost:80) is cross-origin to the Pi (epic 10 T2)
    res.setHeader("Access-Control-Allow-Origin", "*");
    if (req.method === "OPTIONS") {
      res.writeHead(204, {
        "Access-Control-Allow-Methods": "GET, OPTIONS",
        "Access-Control-Allow-Headers": "*"
      });
      res.end();
      return;
    }
    const u = new URL(req.url, "http://localhost");
    if (u.pathname === "/api/v1/capabilities") {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(CAPS);
      return;
    }
    const m = u.pathname.match(IMG_RE);
    if (m) {
      let src;
      try {
        src = decodeURIComponent(m[1]);
      } catch (e) {
        src = m[1];
      }
      if (!/^https?:\/\//.test(src)) {
        res.writeHead(400, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "invalid cdn url in path" }));
        return;
      }
      if (m[2] === "160.jpg" || m[2] === "160") {
        // real pass-through of the upstream CDN image; Sharp based 160px
        // downscaling lands in a later epic 10 task
        const up = http.get(src, (upres) => {
          res.writeHead(upres.statusCode || 502, {
            "Content-Type": upres.headers["content-type"] || "application/octet-stream"
          });
          upres.pipe(res);
        });
        up.on("error", () => {
          res.writeHead(502, { "Content-Type": "application/json" });
          res.end(JSON.stringify({ error: "upstream fetch failed" }));
        });
        return;
      }
      if (m[2] === "colors") {
        // contract shape per epic 10 T2; placeholder dominant color until
        // Sharp/Canvas extraction lands in a later epic 10 task
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ dominant: [128, 128, 128] }));
        return;
      }
    }
    res.writeHead(404, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "not found" }));
  })
  .listen(PORT, "0.0.0.0", () => {
    console.log("mira compute-server listening on :" + PORT);
  });
'
    B64="$(printf '%s' "$COMPUTE_JS" | base64 | tr -d '\n')" || die "base64 encoding failed"

    # install node if needed + write the service file
    run_ssh "mkdir -p /opt/mira /var/log && (command -v node >/dev/null 2>&1 || { if command -v apt-get >/dev/null 2>&1; then apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends nodejs; elif command -v apk >/dev/null 2>&1; then apk add --no-progress nodejs; elif command -v xbps-install >/dev/null 2>&1; then xbps-install -y nodejs; else echo 'no supported package manager found on pi' >&2; exit 1; fi; }) && printf %s \"$B64\" | base64 -d > /opt/mira/compute-server.js" \
        || die "compute deploy: node install / file write failed"

    # restart the service. NEVER pkill -f: the pattern would match this ssh
    # session's own command line. Restart by pid file.
    run_ssh "export MIRA_PI_MODEL=\"$MODEL\"; if [ -f /opt/mira/compute-server.pid ]; then kill \"\$(cat /opt/mira/compute-server.pid)\" 2>/dev/null || true; sleep 1; fi; nohup node /opt/mira/compute-server.js > /var/log/mira-compute.log 2>&1 < /dev/null & echo \$! > /opt/mira/compute-server.pid" \
        || die "compute deploy: service start failed"

    # health check: process alive + endpoint answers
    if run_ssh "sleep 2; if kill -0 \"\$(cat /opt/mira/compute-server.pid)\" 2>/dev/null; then if command -v curl >/dev/null 2>&1; then curl -sf http://127.0.0.1:8080/api/v1/capabilities; echo; else echo \"ok (no curl)\"; fi; else echo 'compute server not running' >&2; tail -5 /var/log/mira-compute.log 2>/dev/null; exit 1; fi"; then
        log "compute endpoint check passed"
    else
        die "compute endpoint check failed"
    fi
fi

log "provisioning finished: model '$MODEL' -> tier '$TIER'"
# machine-readable result line, parsed by the daemon's /api/setup-pi/status
printf 'RESULT model="%s" tier="%s"\n' "$MODEL" "$TIER"
exit 0
