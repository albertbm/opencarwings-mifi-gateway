#!/bin/sh
# Start ocwgw so it survives the adb session that launched it. This busybox has
# no setsid and no nohup, and adb sends SIGHUP to the process group on exit, so
# the gateway would die with the shell. A disposition of "ignored" survives
# exec, so ignoring HUP here is inherited by the binary and it keeps running.
#
# stdin has to leave the pty too. This adbd has no shell_v2, and the legacy
# transport waits for pty EOF before returning, so a child still holding the
# slave fd hangs the adb session even though the gateway itself is running.
trap '' HUP
/online/ocwgw < /dev/null > /online/ocwgw.log 2>&1 &
