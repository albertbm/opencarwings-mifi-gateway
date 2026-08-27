#!/bin/sh
# Start ocwgw so it survives the adb session that launched it. This busybox has
# no setsid and no nohup, and adb sends SIGHUP to the process group on exit, so
# the gateway would die with the shell. A disposition of "ignored" survives
# exec, so ignoring HUP here is inherited by the binary and it keeps running.
trap '' HUP
/online/ocwgw > /online/ocwgw.log 2>&1 &
