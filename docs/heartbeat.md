# Heartbeat and monitoring

Nothing on the opencarwings side will tell you the gateway has gone away. The server sends a
wake-up by handing it to a channel group and returning `True`, whether or not a gateway is
listening. A dead gateway looks exactly like a working one until a car fails to answer.

The heartbeat fixes that from this end. `ocwgw` pushes a short status line out to a URL you
choose, on a timer. Something on the other side watches for the pushes to stop and tells you.
It is off unless you set a URL, so nobody's install changes behaviour by upgrading.

## Shape

Outbound only. The modem is on mobile with a changing public address and no way in, so the
device has to do the talking. One `GET` per beat:

```
GET <beat_url>?status=up&msg=<line>&ping=<n>
```

Those three parameters are what [Uptime Kuma](https://github.com/louislam/uptime-kuma) push
monitors read. They are also harmless to anything else, so the same URL works against
healthchecks.io or a plain nginx location that returns 204 and logs the request.

A `POST` body is not used. Kuma discards it, and relying on a body would tie the format to
one receiver.

## The status line

`msg` is what reaches you in a Telegram notification, so it is written to be read by a
person, not parsed:

```
up 3d04h dev 5d02h · ws=connected · at=ok · sent 12/0 · last "PDU sent" 2h ago
```

When something is wrong, the fault leads and the most recent error is appended:

```
ws=disconnected · at=ok · up 0d02h dev 5d02h · sent 12/1 · err "dial failed: i/o timeout"
```

Every field already exists in `gwStatus` and on `/status.json`. Nothing new is measured:

| Field in `msg` | Source                          | Why it is there                          |
| -------------- | ------------------------------- | ---------------------------------------- |
| `up`           | `uptime_sec`                    | how long this process has been running   |
| `dev`          | `/proc/uptime`                  | separates a gateway restart from a modem reboot |
| `ws=`          | `ws_state`                      | the websocket to opencarwings            |
| `at=`          | `at_ok`                         | the AT channel answering                 |
| `sent`         | `sent_ok` / `sent_err`          | messages out since start                 |
| `last`         | `last_send`, with age           | proof the SMS path has actually worked   |
| `err`          | `last_event`, only when set     | the one line worth having                |

`up` and `dev` diverging is the useful signal. The process restarting on its own is a
different problem from the modem power cycling, and today you cannot tell them apart.

Keep the line under about 200 characters. It has to survive a URL, an nginx request line and
a Telegram message.

## status and ping

`status` is `up` when the websocket reads `connected` **and** the AT channel is answering,
`down` otherwise. A gateway with a live websocket and a wedged modem cannot send an SMS, so
reporting it up would be a lie that holds right until you need it.

`ping` is a number Kuma graphs. It carries **websocket reconnects in the last hour**. It sits
at zero when things are healthy and draws a staircase when the link is flapping, which reads
at a glance in a way a latency figure would not.

## When beats are sent

State is examined every 10 seconds. That is not how often anything is pushed, only how
quickly a change can be noticed. A push happens on two triggers:

* The interval elapsing, `beat_interval`, default 5 minutes.
* A change of up or down. This is the only path that delivers an explanation, so it matters
  more than the timer does.

Three rules keep that from becoming a nuisance:

* **A fault has to hold for 60 seconds** before it is called down, the same way the modem
  check already avoids flapping on a single slow reply. A blip that clears is never
  reported.
* **At most one change beat per 60 seconds.** A flapping websocket cannot turn into a
  stream of requests.
* **Nothing at all for the first 2 minutes after startup, unless it comes up first.** A
  gateway that has just booted is down by definition for a few seconds. Without this you
  would get a down alert on every restart. If it never comes up, the down beat goes out at
  the end of that window.

A beat that fails is logged and dropped. There is no retry inside a tick and no queue, and a
failed beat can never mark the gateway unhealthy or trigger a reconnect. What a failure does
not do is move the remembered state: a change that did not go out is tried again on the next
check rather than being lost.

## Configuration

| Setting         | Web page      | Config file       | Environment          | Default    |
| --------------- | ------------- | ----------------- | -------------------- | ---------- |
| Beat URL        | yes           | `beat_url`        | `OCW_BEAT_URL`       | empty, off |
| Beat interval   | yes           | `beat_interval`   | `OCW_BEAT_INTERVAL`  | `5m`       |

Both live in `/online/ocw_gw.conf` next to the server URL and the enable flag, and take
effect without a restart. Clearing the URL on the page switches the heartbeat off. Anything
under a minute is clamped to a minute, because this runs on a metered SIM.

The same values turn up on `/status.json`, along with what happened to the last push:

```json
"beat_url": "https://kuma.example/api/push/tok3n",
"beat_ok": true,
"beat_last": "up 3d04h dev 5d02h · ws=connected · at=ok · sent 12/0",
"beat_age_sec": 41,
"ws_reconnects_1h": 0,
"device_uptime_sec": 442980
```

The receiver has to present a certificate the embedded CA bundle already trusts. The modem
has no certificate store, so the bundle in the binary is the only root pool. A public CA is
fine. A self-signed certificate means rebuilding the bundle, see
[building.md](building.md).

## What it costs

The beat is cheaper than the websocket keepalive the gateway already runs.

| Traffic                                | Per beat | Per month |
| -------------------------------------- | -------- | --------- |
| Websocket ping, already running, 30s   | ~170 B   | ~15 MB    |
| Beat at 5 min, connection reused       | ~750 B   | ~6 MB     |
| Beat at 5 min, fresh TLS every time    | ~4.5 KB  | ~39 MB    |
| Beat at 15 min, connection reused      | ~750 B   | ~2 MB     |

Reuse one `http.Client` with keep-alives on. Almost all of the cost of a beat is the TLS
handshake, and a reused connection skips it. These are figures worked out from packet sizes
rather than measured on a device, so check them once it is running.

CPU is one handshake every five minutes, tens of milliseconds on an ARMv7 core, less when the
connection is reused. Memory is one goroutine and the TLS buffers for a single connection,
tens of kilobytes. No new dependencies: `crypto/tls` and `net/http` are already in the binary
for `wss://` and the status page, so the binary grows by a few kilobytes of code.

## Watching it with Uptime Kuma

| Setting            | Value                                            |
| ------------------ | ------------------------------------------------ |
| Monitor type       | Push                                             |
| Friendly name      | something you will recognise in a phone alert    |
| Heartbeat interval | 300, matching `beat_interval`                    |
| Retries            | 2, so you hear about it ~15 minutes after it stops |
| Resend every       | 12 for an hourly nag, 0 for one message          |

For Telegram: talk to `@BotFather`, `/newbot`, copy the token. Send your new bot a message,
because a bot cannot start a conversation. Then in Kuma, Settings, Notifications, Telegram,
paste the token and use Auto Get for the chat ID.

Kuma sends `[name] [Down] No heartbeat in the request time.` when the pushes stop, and a
matching Up when they come back. It only notifies on a change of state, so a `msg` that
differs every beat costs nothing extra.

Run the monitor somewhere other than the modem's network. A home power cut or an ISP outage
should still be able to reach you.

## What this cannot tell you

Worth being clear about, because it shapes what the beat is for.

* **A dead device sends nothing.** When the modem loses power there is no last message and no
  explanation. You learn that it stopped, and the time it stopped. That is all push can ever
  give you.
* **Silence has several causes.** No beats means the gateway is down, the modem is off, the
  data connection has gone, or the receiver is unreachable. The beat cannot separate them.
* **The opencarwings websocket is a separate question.** `ws=` in the line is the gateway's
  own view of it. If beats arrive saying `ws=disconnected`, the modem is fine and the problem
  is registration, the server, or the path between them.

## Left out on purpose

* **Shipping log lines in the beat.** They can be stuffed in the query string for nginx to
  record, and it works, but the result is URL-encoded, capped by the request line limit, and
  unreadable without a decoder. The single `err` field carries most of the value.
* **Signal strength from `AT+CSQ`.** Useful context for a failed send, and the only part of
  this design that would touch the AT channel the SMS path depends on. Leave it until the
  rest has been quiet for a while.
* **Anything that can block.** The beat runs in its own goroutine with a hard timeout, reads
  a snapshot of the status, and holds no lock across the request. It must not be able to
  stall the websocket loop or the modem.

A small event log on the device, capped and truncated in place, is the right companion to
this rather than part of it. It survives a reboot, which is exactly when you want to know
what happened, and it is the thing the beat cannot deliver.
