#!/bin/sh
# Install ocwgw onto a rooted Balong MiFi and set it to start on boot.
# Run this from a machine that has adb and can reach the modem.
#
#   scripts/install.sh              # download the latest release binary
#   scripts/install.sh path/to/bin  # use a local binary instead (e.g. one you built)
#
# Override the adb command or device with the ADB and DEV environment variables.
set -e

REPO="albertbm/opencarwings-mifi-gateway"
ASSET="ocwgw-arm"
ADB="${ADB:-adb}"
DEV="${DEV:-192.168.8.1:5555}"

if [ -n "$1" ]; then
	BIN="$1"
	[ -f "$BIN" ] || { echo "binary not found: $BIN" >&2; exit 1; }
else
	BIN="$(mktemp)"
	trap 'rm -f "$BIN"' EXIT
	URL="https://github.com/$REPO/releases/latest/download/$ASSET"
	echo "downloading latest release: $URL"
	curl -fSL "$URL" -o "$BIN" || { echo "download failed" >&2; exit 1; }
fi

$ADB connect "$DEV" >/dev/null 2>&1 || true

echo "pushing ocwgw to /online/ocwgw"
$ADB push "$BIN" /online/ocwgw >/dev/null
$ADB shell "chmod 755 /online/ocwgw"

# Push the boot snippet as a file so we do not have to fight shell quoting.
tmp="$(mktemp)"
cat > "$tmp" <<'SNIP'

# ocwgw
[ -x /online/ocwgw ] && ( sleep 20; /online/ocwgw > /online/ocwgw.log 2>&1 ) &
SNIP
$ADB push "$tmp" /online/ocwgw-autorun.snip >/dev/null
rm -f "$tmp"

echo "adding ocwgw to /system/etc/autorun.sh (if not already there)"
$ADB shell 'grep -q ocwgw /system/etc/autorun.sh 2>/dev/null && echo "already set up" || { mount -o remount,rw /system; cat /online/ocwgw-autorun.snip >> /system/etc/autorun.sh; mount -o remount,ro /system; echo "added to autorun"; }'

echo
echo "done."
echo "start it now:  $ADB shell /online/ocwgw   (or reboot the modem)"
echo "then open http://192.168.8.1:8080 for the device_id and key to register."
