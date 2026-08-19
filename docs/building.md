# Building it yourself

Each release ships a prebuilt ARMv7 binary. If you have the same kind of modem, grab that
from the [releases](https://github.com/albertbm/opencarwings-mifi-gateway/releases) and
skip this page. Build from source when you want to change something, target a different
CPU, or would rather not trust a prebuilt binary.

## What you need

* Go 1.21 or newer, on any machine. It cross compiles, so you do not build on the modem.
* Nothing else. The binary is static (`CGO_ENABLED=0`).

## Find your modem's CPU

Pull a system binary off the modem and look at it:

```
adb pull /system/bin/busybox /tmp/b && file /tmp/b
```

On the E5577 this says `ELF 32-bit ARM, EABI5, statically linked`, which is `GOARCH=arm GOARM=7`.

## Build

ARMv7 (the E5577 and most Balong MiFis of that era):

```
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "-s -w" -o ocwgw-arm ./cmd/ocwgw
```

For another CPU, change `GOARCH` (and `GOARM`):

```
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w" -o ocwgw-arm ./cmd/ocwgw        # 64-bit ARM
GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go build -ldflags "-s -w" -o ocwgw-arm ./cmd/ocwgw   # older ARMv5
```

The binary is static, so it runs on the modem's bionic userland with no shared libraries.
To refresh the embedded CA bundle, copy your system's over `cmd/ocwgw/ca-certificates.crt`,
for example from `/etc/ssl/certs/ca-certificates.crt`.

## Environment overrides

The server URL and the enable switch are set from the web page, so most people never touch
these. They matter on a different modem with a different AT device path, or when launching
from a script.

| Variable        | Default                                      | Purpose                                   |
| --------------- | -------------------------------------------- | ----------------------------------------- |
| `OCW_WS_URL`    | `wss://opencarwings.viaaq.eu/ws/smsgateway/` | server websocket                          |
| `OCW_APPVCOM`   | `/dev/appvcom1`                              | AT channel to open, `appvcom` is HiLink's |
| `OCW_IDS`       | `/online/ocw_gw.ids`                         | where identity is stored                  |
| `OCW_CONF`      | `/online/ocw_gw.conf`                        | where url and enable flag live            |
| `OCW_HTTP_ADDR` | `:8080`                                      | status page listen address                |

## Implementation notes

A few non-obvious things about how the code is put together:

* Go's `os.File` uses the runtime poller (epoll) on the fd. `/dev/appvcom1` does not get on
  with that and reads come back empty. Raw blocking `syscall.Read`/`Write` fixes it, which
  is why the modem layer uses syscalls directly.
* The modem has no CA certificate store, so a normal TLS dial fails to verify. The bundle
  is embedded in the binary with `//go:embed` and set as the only root pool.
* `/dev/appvcom1` is a shared AT channel that also emits unsolicited reports (`^SYSINFOEX`,
  `^DSFLOWRPT` and so on). The reader keeps a rolling buffer and each command scans from a
  mark for its own terminator, so the noise is harmless.
