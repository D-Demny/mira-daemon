#!/bin/sh
# setup-tethering.sh - USB-tethering setup for the Raspberry Pi (epic 10 ticket10-6, part A)
#
# Runs ON THE DEVICE (armv7l, POSIX sh / busybox compatible) and configures
# USB tethering on the USB-connected Raspberry Pi over SSH (key-first, the
# same run_ssh schema as setup-pi.sh):
#   1. connection probe against the Pi
#   2. uplink detection on the RPi: eth (Ethernet) vs wlan (WiFi) vs none
#   3. tethering setup on the RPi: the RPi becomes the tethering router for
#      the Mira's USB segment (192.168.7.0/24) -
#        - USB interface found (host side: the Mira's gadget appears as
#          usb0; device side: the RPi's own g_ether gadget is enabled as a
#          best effort, which creates usb0)
#        - static ip 192.168.7.1/24 on the USB segment (the mira expects the
#          pi at 192.168.7.1, the UI default)
#        - dnsmasq (dedicated config file) = DHCP + DNS for the mira
#        - NAT (masquerade) from the USB segment to the uplink, ip_forward -
#          only when the uplink has a usable ip; the uplink's own
#          configuration is NEVER touched, every step is idempotent
#   4. internet test: rpi-side (uplink alive?) and device-side (the mira
#      itself gets internet over usb - the authoritative check)
#
# Uplink-detection strategy (distribution-agnostic: Debian / Raspberry Pi OS
# / DietPi): the first interface (alphabetical, so Ethernet wins over WLAN
# when both are online) that matches eth*/en*/wlan*/wl*, is not loopback and
# holds a usable IPv4 address (not 169.254 link-local, NOT the usb tethering
# segment 192.168.7.0/24 itself) - checked with iproute2; fallback without
# iproute2: /sys/class/net operstate. The usb segment itself must never be
# mistaken for the uplink (name filter + the 192.168.7.x exclusion).
#
# Environment (set by the daemon's /api/pi/tethering, or manually when run
# by hand):
#   SSH_HOST  Pi IP address (required)
#   SSH_USER  ssh user on the Pi (required)
#   SSH_PASS  ssh password (OPTIONAL - the run is key-first, the password is
#             only the fallback when key auth fails; without key AND without
#             password the script refuses to start)
#   MIRA_SSH_KEY_PATH  path of the device ssh key for the key-first attempt
#                      (default /etc/mira/ssh/id_ed25519)
#
# On completion the script prints the machine-readable line
#   RESULT uplink="eth|wlan|none" tethering="ok|fail" internet="ok|fail" detail="..."
# and exits 0 only when tethering=ok AND internet=ok (a run that finished
# but could not deliver internet still prints the RESULT line, so the daemon
# can report the machine-readable fields; the daemon maps the non-zero exit
# to state=failed and parses the RESULT line either way).

set -u

SSH_HOST="${SSH_HOST:-}"
SSH_USER="${SSH_USER:-}"
SSH_PASS="${SSH_PASS:-}"

log() {
    printf '[setup-tethering] %s\n' "$*"
}

die() {
    printf '[setup-tethering] ERROR: %s\n' "$*" >&2
    exit 1
}

[ -n "$SSH_HOST" ] || die "SSH_HOST env var is required"
[ -n "$SSH_USER" ] || die "SSH_USER env var is required"
command -v ssh >/dev/null 2>&1 || die "ssh client is not installed on this device"

# key-first SSH (same schema as setup-pi.sh): the device key first (BatchMode,
# no inherited agent so a foreign key cannot mask our key), and only an ssh
# exit 255 (connection/authentication level) may fall back to the password -
# any other exit code is the REMOTE command's own result and is returned as-is
# (a password retry would double-execute it).
MIRA_SSH_KEY_PATH="${MIRA_SSH_KEY_PATH:-/etc/mira/ssh/id_ed25519}"
if [ ! -f "$MIRA_SSH_KEY_PATH" ] && [ -z "$SSH_PASS" ]; then
    die "no ssh key at $MIRA_SSH_KEY_PATH and no SSH_PASS (run the provisioning wizard /api/setup-pi first)"
fi
if [ -n "$SSH_PASS" ]; then
    command -v sshpass >/dev/null 2>&1 || die "sshpass is not installed on this device (password fallback requested)"
    # sshpass -e reads the password from the SSHPASS env var so it never
    # shows up in the process list (sshpass -p would)
    export SSHPASS="$SSH_PASS"
fi

run_ssh_key() {
    SSH_AUTH_SOCK= ssh -i "$MIRA_SSH_KEY_PATH" \
        -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
        -o ConnectTimeout=10 -o LogLevel=ERROR "$SSH_USER@$SSH_HOST" "$1"
}

run_ssh_pass() {
    sshpass -e ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=10 -o LogLevel=ERROR "$SSH_USER@$SSH_HOST" "$1"
}

run_ssh() {
    if [ -f "$MIRA_SSH_KEY_PATH" ]; then
        run_ssh_key "$1"
        rc=$?
        if [ "$rc" -ne 255 ]; then
            return "$rc"
        fi
        # rc 255: connection/authentication level - fall through
    fi
    if [ -n "$SSH_PASS" ]; then
        run_ssh_pass "$1"
        return $?
    fi
    return 255
}

# device-side internet probe (the mira's own network, i.e. the usb segment
# once tethering works): curl -> wget -> nc -> bash /dev/tcp (the device
# /bin/sh is bash, so the last resort always works there)
dev_internet_ok() {
    if command -v curl >/dev/null 2>&1; then
        curl -sf -m 5 -o /dev/null http://1.1.1.1/ 2>/dev/null && return 0
    elif command -v wget >/dev/null 2>&1; then
        wget -q -T 5 -t 1 -O /dev/null http://1.1.1.1/ 2>/dev/null && return 0
    elif command -v nc >/dev/null 2>&1; then
        nc -z -w 5 1.1.1.1 443 2>/dev/null && return 0
    fi
    if (exec 3<>/dev/tcp/1.1.1.1/443) 2>/dev/null; then
        exec 3>&- 3<&- 2>/dev/null
        return 0
    fi
    return 1
}

log "usb tethering setup for $SSH_HOST (user: $SSH_USER)"

# ---------------------------------------------------------------- step 1:
# connection probe against the Pi (key auth must work - the run is a
# key-first run by contract, the password is only the fallback)
log "step 1/4: connection probe on $SSH_HOST:22"
run_ssh true || die "cannot ssh to $SSH_USER@$SSH_HOST (key auth failed - run the provisioning wizard /api/setup-pi first)"
log "ssh connection ok"

# ---------------------------------------------------------------- step 2:
# uplink detection on the rpi (the remote program is a single-quoted block:
# no single quotes and no local shell expansion inside - everything is
# evaluated on the pi)
log "step 2/4: detecting the rpi uplink (eth vs wlan)"
DETECT_PROG='
# MIRA_TETHER_DETECT
# usable ipv4 on interface $1: not link-local (169.254.*) and not the usb
# tethering segment (192.168.7.*) - the tethering link is never the uplink
has_ip() {
    ip -4 addr show dev "$1" 2>/dev/null | grep -v 169.254. | grep -v 192.168.7. | grep -q inet
}
up=none
if command -v ip >/dev/null 2>&1; then
    for d in /sys/class/net/*; do
        ifc=$(basename "$d")
        [ "$ifc" = lo ] && continue
        case "$ifc" in
            eth*|en*|wlan*|wl*) ;;
            *) continue ;;
        esac
        has_ip "$ifc" || continue
        case "$ifc" in
            eth*|en*) up=eth; break ;;
            *) up=wlan; break ;;
        esac
    done
fi
if [ "$up" = none ]; then
    # fallback without iproute2: operstate (no address check possible)
    for d in /sys/class/net/*; do
        ifc=$(basename "$d")
        [ "$ifc" = lo ] && continue
        case "$ifc" in
            eth*|en*|wlan*|wl*) ;;
            *) continue ;;
        esac
        [ -e "$d/operstate" ] || continue
        [ "$(cat "$d/operstate" 2>/dev/null)" = up ] || continue
        case "$ifc" in
            eth*|en*) up=eth; break ;;
            *) up=wlan; break ;;
        esac
    done
fi
printf "UP=%s\n" "$up"
'
UP_OUT="$(run_ssh "$DETECT_PROG")" || die "uplink detection failed (ssh error)"
UPLINK=none
case "$UP_OUT" in
    *"UP=eth"*) UPLINK=eth ;;
    *"UP=wlan"*) UPLINK=wlan ;;
esac
log "detected uplink: $UPLINK"

# ---------------------------------------------------------------- step 3:
# tethering setup on the rpi (usb segment + dhcp + nat). The remote program
# is self-contained (re-detects the interfaces it needs) and idempotent.
log "step 3/4: configuring usb tethering on the rpi"
SETUP_PROG='
# MIRA_TETHER_SETUP
set -u
log() { echo "mira-tether: $*"; }

# 1) the usb interface facing the mira: pre-existing usb0/usb1 (the rpi is
#    the host and the miras gadget shows up) or eth1/eth2 (usb ethernet on
#    some kernels). If nothing exists yet, the rpi may be the USB GADGET
#    side (the mira is the host and powers the rpi): enable the gadget
#    ethernet function (g_ether) and wait for usb0 to appear.
usbif=""
for cand in usb0 usb1 eth1 eth2; do
    [ -e "/sys/class/net/$cand" ] && usbif=$cand && break
done
if [ -z "$usbif" ] && [ -d /sys/class/udc ]; then
    modprobe g_ether 2>/dev/null || true
    i=0
    while [ -z "$usbif" ] && [ $i -lt 12 ]; do
        for cand in usb0 usb1 eth1 eth2; do
            [ -e "/sys/class/net/$cand" ] && usbif=$cand && break
        done
        [ -z "$usbif" ] && { sleep 1; i=$((i+1)); }
    done
fi
if [ -z "$usbif" ]; then
    log "no usb network interface found facing the mira (cable connected?)"
    echo "USB=none"
    exit 1
fi
log "usb interface: $usbif"

# 2) static ip on the usb segment (the mira expects the pi at 192.168.7.1,
#    the ui default); idempotent, an existing 192.168.7.1 is kept
if command -v ip >/dev/null 2>&1; then
    ip link set dev "$usbif" up 2>/dev/null || true
    if ! ip -4 addr show dev "$usbif" | grep -q "192\.168\.7\.1/"; then
        ip addr add 192.168.7.1/24 dev "$usbif" 2>/dev/null \
            || log "WARNING: could not assign 192.168.7.1/24 to $usbif"
    fi
else
    log "WARNING: no iproute2 (ip) on the rpi - skipping static ip assignment"
fi

# 3) dhcp + dns for the mira on the usb segment: dnsmasq with a dedicated,
#    clearly marked config file (installed via apt when missing -
#    debian/dietpi/raspberry pi os). The file is rewritten on every run
#    (idempotent) and scoped to the usb segment.
dhcp=fail
if command -v dnsmasq >/dev/null 2>&1; then
    :
elif command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq >/dev/null 2>&1 || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends dnsmasq >/dev/null 2>&1 || true
fi
if command -v dnsmasq >/dev/null 2>&1; then
    mkdir -p /etc/dnsmasq.d
    {
        echo "# managed by setup-tethering.sh (mira usb tethering) - do not edit"
        echo "interface=$usbif"
        echo "listen-address=192.168.7.1"
        echo "bind-interfaces"
        echo "dhcp-range=192.168.7.2,192.168.7.50,255.255.255.0,12h"
        echo "dhcp-option=3,192.168.7.1"
        echo "dhcp-option=6,192.168.7.1"
        echo "dhcp-authoritative"
    } > /etc/dnsmasq.d/99-mira-usb.conf
    if command -v systemctl >/dev/null 2>&1; then
        if systemctl is-active dnsmasq >/dev/null 2>&1; then
            systemctl restart dnsmasq >/dev/null 2>&1 || log "WARNING: dnsmasq restart failed"
        else
            systemctl enable --now dnsmasq >/dev/null 2>&1 || log "WARNING: dnsmasq start failed"
        fi
    elif command -v service >/dev/null 2>&1; then
        service dnsmasq restart >/dev/null 2>&1 || service dnsmasq start >/dev/null 2>&1 || log "WARNING: dnsmasq start failed"
    fi
    dhcp=ok
else
    log "WARNING: dnsmasq unavailable - the mira needs a static ip on the usb segment"
fi

# 4) nat from the usb segment to the uplink - only when the uplink has a
#    usable address (re-detected here, same logic as the detect step; the
#    usb segment is excluded by the 192.168.7. filter). The uplink interface
#    itself is never reconfigured. ip_forward is set at runtime AND persisted
#    (our own sysctl file). The iptables rules are runtime only - re-running
#    this script (the reboot flow, ticket10-6 part B) restores them after a
#    rpi reboot.
nat=skip
find_uplink_if() {
    if command -v ip >/dev/null 2>&1; then
        for d in /sys/class/net/*; do
            ifc=$(basename "$d")
            [ "$ifc" = lo ] && continue
            case "$ifc" in
                eth*|en*|wlan*|wl*) ;;
                *) continue ;;
            esac
            ip -4 addr show dev "$ifc" 2>/dev/null | grep -v 169.254. | grep -v 192.168.7. | grep -q inet || continue
            printf "%s" "$ifc"
            return
        done
    fi
    for d in /sys/class/net/*; do
        ifc=$(basename "$d")
        [ "$ifc" = lo ] && continue
        case "$ifc" in
            eth*|en*|wlan*|wl*) ;;
            *) continue ;;
        esac
        [ -e "$d/operstate" ] || continue
        [ "$(cat "$d/operstate" 2>/dev/null)" = up ] || continue
        printf "%s" "$ifc"
        return
    done
}
upif=$(find_uplink_if)
if [ -n "$upif" ] && command -v iptables >/dev/null 2>&1; then
    echo 1 > /proc/sys/net/ipv4/ip_forward 2>/dev/null || true
    printf "net.ipv4.ip_forward = 1\n" > /etc/sysctl.d/99-mira-usb.conf 2>/dev/null || true
    if ! iptables -t nat -C POSTROUTING -s 192.168.7.0/24 -o "$upif" -j MASQUERADE 2>/dev/null; then
        if ! iptables -t nat -A POSTROUTING -s 192.168.7.0/24 -o "$upif" -j MASQUERADE 2>/dev/null; then
            log "WARNING: could not add nat rule (permissions? firewall?)"
        fi
    fi
    if ! iptables -C FORWARD -s 192.168.7.0/24 -j ACCEPT 2>/dev/null; then
        iptables -A FORWARD -s 192.168.7.0/24 -j ACCEPT 2>/dev/null || true
    fi
    if ! iptables -C FORWARD -d 192.168.7.0/24 -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
        iptables -A FORWARD -d 192.168.7.0/24 -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
    fi
    nat=ok
    log "nat configured on uplink $upif"
fi
if [ "$nat" = skip ] && [ -n "$upif" ]; then
    log "WARNING: no iptables on the rpi - no nat"
fi
if [ "$nat" = skip ] && [ -z "$upif" ]; then
    log "note: no uplink with a usable address - nat skipped"
fi

echo "USB=$usbif"
echo "DHCP=$dhcp"
echo "NAT=$nat"
exit 0
'
SETUP_OUT="$(run_ssh "$SETUP_PROG")"
SETUP_RC=$?
USBIF=$(printf '%s\n' "$SETUP_OUT" | grep "^USB=" | cut -d= -f2 | head -n1)
[ -n "$USBIF" ] || USBIF=none
DHCP=$(printf '%s\n' "$SETUP_OUT" | grep "^DHCP=" | cut -d= -f2 | head -n1)
[ -n "$DHCP" ] || DHCP=fail
NAT=$(printf '%s\n' "$SETUP_OUT" | grep "^NAT=" | cut -d= -f2 | head -n1)
[ -n "$NAT" ] || NAT=skip
TETHERING=fail
if [ $SETUP_RC -eq 0 ] && [ "$USBIF" != none ]; then
    TETHERING=ok
else
    log "tethering setup reported a problem (rc=$SETUP_RC)"
fi
log "tethering: usb=$USBIF dhcp=$DHCP nat=$NAT"

# ---------------------------------------------------------------- step 4:
# internet test. Rpi-side (uplink alive?) runs remotely; device-side (the
# mira itself, the authoritative check) runs locally - the mira may still be
# taking its dhcp lease, so the probe retries briefly.
log "step 4/4: internet test"
RPINET_PROG='
# MIRA_TETHER_RPINET
if command -v curl >/dev/null 2>&1; then
    if curl -sf -m 5 -o /dev/null http://1.1.1.1/ 2>/dev/null; then
        echo "RPI_NET=ok"
        exit 0
    fi
elif command -v wget >/dev/null 2>&1; then
    if wget -q -T 5 -t 1 -O /dev/null http://1.1.1.1/ 2>/dev/null; then
        echo "RPI_NET=ok"
        exit 0
    fi
fi
if command -v nc >/dev/null 2>&1; then
    if nc -z -w 5 1.1.1.1 443 2>/dev/null; then
        echo "RPI_NET=ok"
        exit 0
    fi
fi
if (exec 3<>/dev/tcp/1.1.1.1/443) 2>/dev/null; then
    exec 3>&- 3<&- 2>/dev/null
    echo "RPI_NET=ok"
    exit 0
fi
echo "RPI_NET=fail"
exit 1
'
RPI_NET=fail
RPI_OUT="$(run_ssh "$RPINET_PROG")" || true
case "$RPI_OUT" in
    *"RPI_NET=ok"*) RPI_NET=ok ;;
esac

INTERNET=fail
if [ -n "${FAKE_TETHER_DEV_NET:-}" ]; then
    # test hook (daemon smoke tests only, never set in production): skip the
    # real device-side probes so the run stays deterministic
    [ "$FAKE_TETHER_DEV_NET" = ok ] && INTERNET=ok
else
    i=0
    while [ $i -lt 6 ]; do
        if dev_internet_ok; then
            INTERNET=ok
            break
        fi
        sleep 2
        i=$((i+1))
    done
fi
if [ "$INTERNET" = fail ] && [ "$RPI_NET" = ok ]; then
    log "WARNING: the rpi has internet but the device-side probe failed (usb link or route problem)"
fi
log "internet: rpi=$RPI_NET device=$INTERNET"

# machine-readable result line, parsed by the daemon's /api/pi/tethering/status
DETAIL="usb=$USBIF dhcp=$DHCP nat=$NAT rpinet=$RPI_NET"
if [ "$TETHERING" = ok ] && [ "$INTERNET" = ok ]; then
    log "usb tethering finished: uplink=$UPLINK usb=$USBIF internet ok"
    printf 'RESULT uplink="%s" tethering="ok" internet="ok" detail="%s"\n' "$UPLINK" "$DETAIL"
    exit 0
fi
printf 'RESULT uplink="%s" tethering="%s" internet="%s" detail="%s"\n' "$UPLINK" "$TETHERING" "$INTERNET" "$DETAIL"
exit 1
