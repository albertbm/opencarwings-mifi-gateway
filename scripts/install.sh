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

# Written out rather than pushed from the repo, so this works when the script is
# piped straight from curl and $0 is just "sh". Keep in step with
# scripts/start-ocwgw.sh. HUP is ignored so the gateway survives the adb session,
# and stdin leaves the pty so the session itself does not hang.
echo "pushing start-ocwgw.sh to /online/start-ocwgw.sh"
tmp="$(mktemp)"
cat > "$tmp" <<'LAUNCH'
#!/bin/sh
trap '' HUP
/online/ocwgw < /dev/null > /online/ocwgw.log 2>&1 &
LAUNCH
$ADB push "$tmp" /online/start-ocwgw.sh >/dev/null
rm -f "$tmp"
$ADB shell "chmod 755 /online/start-ocwgw.sh"

# Push the boot snippet as a file so we do not have to fight shell quoting.
tmp="$(mktemp)"
cat > "$tmp" <<'SNIP'

# opencarwings gateway
[ -x /online/ocwgw ] && ( sleep 20; /online/ocwgw > /online/ocwgw.log 2>&1 ) &
SNIP
$ADB push "$tmp" /online/ocwgw-autorun.snip >/dev/null
rm -f "$tmp"

# The marker has to match autorunMarker in cmd/ocwgw/main.go, or the binary's own
# autostart check does not see the line this wrote. Installs from before that was
# aligned used "# ocwgw"; appending over one of those would give the modem two boot
# entries.
#
# The decision is made here rather than on the modem: this adbd predates shell_v2,
# so `adb shell` exits 0 whatever the remote command did, and a remote `exit 1`
# would be swallowed. The state comes back as a word instead. Matched with a
# wildcard because the legacy pty transport appends CR.
echo "checking /system/etc/autorun.sh"
state="$($ADB shell 'if grep -q "# opencarwings gateway" /system/etc/autorun.sh 2>/dev/null; then echo OCWGW_NEW; elif grep -q ocwgw /system/etc/autorun.sh 2>/dev/null; then echo OCWGW_OLD; else echo OCWGW_NONE; fi')"

case "$state" in
*OCWGW_NEW*)
	echo "already set up"
	;;
*OCWGW_OLD*)
	echo >&2
	echo "An older ocwgw autorun line is already there, under the previous marker." >&2
	echo "Adding another would give the modem two boot entries, so nothing was changed." >&2
	echo >&2
	echo "/system is read-only and this busybox has no sed, so clean it from here:" >&2
	echo >&2
	echo "  $ADB pull /system/etc/autorun.sh autorun.sh" >&2
	echo "  grep -v ocwgw autorun.sh > autorun.clean" >&2
	echo "  $ADB shell 'mount -o remount,rw /system'" >&2
	echo "  $ADB push autorun.clean /system/etc/autorun.sh" >&2
	echo "  $ADB shell 'chmod 755 /system/etc/autorun.sh; mount -o remount,ro /system'" >&2
	echo >&2
	echo "Then run this again. The binary and launcher are already in place." >&2
	exit 1
	;;
*OCWGW_NONE*)
	# Chained, so a failed remount cannot report success.
	$ADB shell 'mount -o remount,rw /system && cat /online/ocwgw-autorun.snip >> /system/etc/autorun.sh && mount -o remount,ro /system && echo "added to autorun" || echo "FAILED to write autorun.sh"'
	;;
*)
	echo "could not read /system/etc/autorun.sh (got: $state)" >&2
	exit 1
	;;
esac

echo
echo "done."
echo "start it now:  $ADB shell /online/start-ocwgw.sh   (or reboot the modem)"
echo "then open http://192.168.8.1:8080 for the device_id and key to register."
