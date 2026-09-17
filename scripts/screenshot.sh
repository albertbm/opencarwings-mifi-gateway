#!/bin/sh
# Save a shot of the modem's panel. The gateway draws the frame when asked, so
# this works whether or not the screen is lit.
#
#   scripts/screenshot.sh [host] [file]
#
# From a machine that reaches the modem only through another one, pipe it:
#   ssh pi 'curl -s http://192.168.8.1:8080/screen.png' > panel.png
set -e
host=${1:-192.168.8.1:8080}
out=${2:-panel.png}
curl -fsS "http://$host/screen.png" -o "$out"
echo "$out"
