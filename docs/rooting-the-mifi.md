# Rooting a Balong MiFi (short version)

`ocwgw` needs a rooted modem that gives you an adb root shell and an internal AT
device. How you get there is up to you, the app does not care. What follows is what worked
on a Huawei E5577s-321. Other Balong models work much the same way, but the firmware and
the boot point differ, so check a guide for your model.

The gist: flash a modded firmware that ships adb, telnet and the internal AT device turned
on, then talk to it over adb.

## What you should end up with

* `adb connect 192.168.8.1:5555` gives a root shell (`id` shows `uid=0`)
* `/dev/appvcom` and `/dev/appvcom1` exist, the two internal AT channels. `ocwgw` uses
  `/dev/appvcom1` and leaves `/dev/appvcom` to HiLink's data connection
* the modem still has its normal cellular data connection

## E5577s-321, roughly how

1. Open the case and find the boot test point on the board. Shorting it to ground while
   plugging in USB drops the Balong CPU into download mode.
2. In download mode, load a usbloader (the "Max" loader for this chip) with a Balong USB
   downloader, then flash a modded firmware that has adb/telnet/AT enabled. The
   `21.333.01.00.00 _M_AT` build works.
3. On Linux, forth32's `balong_flash` was too old for the R11 monitor on this firmware.
   A newer fork, `urz-7/balong-flash`, flashed it fine and reads the Windows firmware
   `.exe` directly. On Windows the vendor Balong downloader plus the firmware `.exe`
   also works.
4. After flashing you get a battery error screen. Put the battery in, power on, and it
   boots the modded firmware. The stock web UI language may come up Russian. That is just
   the mod, and it does not matter here.

## Useful references

* forth32 balong tools: `balong-usbdload`, `balong-nvtool`, `balong_flash`
* `urz-7/balong-flash` for the R11 capable flasher
* Hovatek and RouterUnlock have E5577 boot-shot guides with board photos
* modded firmware with adb/telnet/AT turns up on the usual modem forums

Once you are rooted and `/dev/appvcom1` is there, go back to the main README and build the app.
