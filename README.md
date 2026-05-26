# go-steam-ws

A fork of [Philipp15b/go-steam v3](https://github.com/Philipp15b/go-steam)
that adds **WebSocket transport** and **custom-dialer** support, so the
library can be used through HTTP-CONNECT / SOCKS5 proxies and reach
Steam's `:443` CM endpoints (those that only speak WebSocket).

The upstream library is itself a Go port of
[SteamKit2](https://github.com/SteamRE/SteamKit), the canonical C#
implementation of Steam's binary protocol.

## What's added vs upstream

| Feature | Upstream `Philipp15b/go-steam/v3` | This fork |
|---|---|---|
| Raw TCP transport (port 27015–27050) | ✅ | ✅ (unchanged) |
| **WebSocket transport** (port 443, `wss://`) | ❌ | ✅ |
| **Custom dialer** (SOCKS5 / HTTP-CONNECT proxy) | ❌ | ✅ |
| Type-aware Steam Directory (`GetCMListForConnect/v1`) | ❌ | ✅ |
| Logon nil-guard for deprecated WebApi nonce | ❌ | ✅ |
| `DebugPackets` toggle for transport bring-up | ❌ | ✅ |

Everything else from upstream — friends, trading, social, TF2, GC
routing, etc. — is preserved and works the same way.

## Installation

```bash
go get github.com/sicdex/go-steam-ws
```

If you want to vendor it inside another module via a sibling `replace`
directive:

```
replace github.com/sicdex/go-steam-ws => ../go-steam-ws
```

## Quick start — direct TCP (legacy)

```go
import steam "github.com/sicdex/go-steam-ws"

c := steam.NewClient()
go func() {
    for evt := range c.Events() {
        // handle ConnectedEvent → call c.Auth.LogOn, etc.
    }
}()
addr, err := c.Connect()
if err != nil { log.Fatal(err) }
log.Printf("connecting to %s", addr)
```

Identical to upstream: random TCP CM is chosen from the directory, the
Steam ChannelEncrypt handshake runs, then `ConnectedEvent` fires and
you call `c.Auth.LogOn(...)`.

## Quick start — WebSocket through a proxy

```go
import (
    "log"
    steam "github.com/sicdex/go-steam-ws"
)

c := steam.NewClient()
c.Dialer = mySOCKS5OrHTTPConnectDialer // see examples/wsclient

// Pick a websocket-capable CM. The directory exposes ~150 WS CMs; if
// your proxy only allows CONNECT to :443, filter to those (~30).
cms, err := steam.FetchCMListForConnect(0)
if err != nil { log.Fatal(err) }
ws := steam.FilterByType(cms, "websockets")
cm := steam.PickRandom(ws)

if err := c.ConnectToWebSocket(cm.Endpoint); err != nil { log.Fatal(err) }
// ConnectedEvent fires immediately (no ChannelEncryptRequest over WS),
// so the next event you handle can call c.Auth.LogOn directly.
```

A full standalone smoke-test is in
[`examples/wsclient/main.go`](examples/wsclient/main.go) — proven
end-to-end through Bright Data datacenter on `:443`:

```bash
go run ./examples/wsclient \
    -user STEAM_USERNAME -pass STEAM_PASSWORD \
    -only-443 \
    -proxy "http://brd-customer-XXX-zone-YYY:PWD@brd.superproxy.io:33335"
```

It also has a `-list` mode to dump the websocket CM list without
attempting login.

## Working with proxies

The `Client.Dialer` field is a `func(network, address string) (net.Conn, error)`
— matches `net.Dial`'s signature. When non-nil it's used for the TCP
leg of every connection (both `dialTCP` for raw TCP CMs and the TCP
leg under the TLS handshake for `dialWebSocket`). Set it BEFORE
`Connect` / `ConnectToWebSocket`.

Two ways the library uses it:

- **TCP CM path** — `dialTCP(laddr, raddr, dialer)`. The dialer just
  opens `tcp` to `raddr.String()`.
- **WebSocket CM path** — `dialWebSocket(host, dialer, timeout)`. The
  dialer opens `tcp` to the CM `host:port`; `gorilla/websocket` then
  performs the TLS+WS upgrade over that proxy-tunneled socket — exactly
  what a corporate HTTPS proxy expects.

`examples/wsclient/main.go` ships ready-to-use SOCKS5 and HTTP CONNECT
dialer implementations if you need a reference.

## WebSocket vs TCP — semantic differences

These are baked into `wsConnection` so you don't have to think about
them, but they matter for debugging:

1. **No `ChannelEncryptRequest` over WS.** TLS terminates the
   confidentiality concern. SteamKit2 deliberately skips its
   `EnvelopeEncryptedConnection` wrapper for WS connections; we follow
   suit. `ConnectToWebSocket` emits `ConnectedEvent` right after the
   WS upgrade so callers can call `Auth.LogOn` immediately.

2. **One WS binary frame = one Steam packet.** No TCP-style length
   prefix or `"VT01"` magic; each `ws.WriteMessage(BinaryMessage, ...)`
   is a single Steam packet, and each `ws.ReadMessage()` returns a
   single Steam packet.

3. **`CMsgProtoBufHeader.routing_appid` is mandatory for GC messages
   over WS.** The TCP-side CM router could route by
   `CMsgGCClient.appid` alone; the WS-side router silently drops
   `EMsg_ClientToGC` packets without `routing_appid` set. This fork's
   `GameCoordinator.Write` always sets it. (See
   [SteamKit2's SteamGameCoordinator.cs](https://github.com/SteamRE/SteamKit/blob/master/SteamKit2/SteamKit2/Steam/Handlers/SteamGameCoordinator/SteamGameCoordinator.cs)
   for the canonical reference.)

## Debugging the wire

When bringing up a new transport or proxy chain, set:

```go
steam.DebugPackets = true
```

before connecting. The library will then print one line per incoming
packet showing `EMsg=<num> (<name>) IsProto=<bool>`. Combine with
`gsbot/PacketLogger` if you also want to capture full packet bodies
to disk.

## Other sub-packages (unchanged from upstream)

- [`gsbot`](https://pkg.go.dev/github.com/sicdex/go-steam-ws/gsbot) — utilities that make writing bots easier
- [`trade`](https://pkg.go.dev/github.com/sicdex/go-steam-ws/trade) — trading
- [`tradeoffer`](https://pkg.go.dev/github.com/sicdex/go-steam-ws/tradeoffer) — trade offers
- [`economy/inventory`](https://pkg.go.dev/github.com/sicdex/go-steam-ws/economy/inventory) — inventories
- [`tf2`](https://pkg.go.dev/github.com/sicdex/go-steam-ws/tf2) — Team Fortress 2 helpers

## Working with go-steam-ws

- If something is not working, first check whether the same operation
  works (under the same conditions) in the official Steam client on
  that account.
- Since Steam does not maintain a public API for most of what this
  library does, things can break when Valve changes the protocol —
  especially `trade` and `tradeoffer`.
- Always gather as much information as possible when filing an issue:
  network conditions, the proxy chain (if any), and `DebugPackets`
  output from the moment things go wrong.
- Take a look at [SteamKit2](https://github.com/SteamRE/SteamKit) and
  [its other ports](https://github.com/SteamRE/SteamKit/wiki/Ports) —
  a fix often lives there already.

## Updating to a new SteamKit version

Go source code is generated from SteamKit's `.steamd` files via the
code in the `generator` directory. See `generator/README.md`.

## License

Steam for Go is licensed under the New BSD License. See
[`LICENSE.txt`](LICENSE.txt).

## Credits

- [SteamRE/SteamKit](https://github.com/SteamRE/SteamKit) — the
  canonical Steam client implementation in C#. The WebSocket transport
  here was written by reading their `WebSocketConnection.cs` and
  `WebSocketContext.cs`.
- [Philipp15b/go-steam](https://github.com/Philipp15b/go-steam) — the
  baseline this fork extends. Everything except WebSocket transport,
  the `Dialer` hook, and the GC `routing_appid` fix is upstream's work.
