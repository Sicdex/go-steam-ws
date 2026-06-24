# local-protos — proto overrides outside the submodule

`generator/Protobufs/` is a **git submodule** tracking upstream
[SteamDatabase/Protobufs](https://github.com/SteamDatabase/Protobufs). We don't
want local edits living inside it (they dirty the submodule, can't be pushed,
and get clobbered on `git submodule update`). So any proto we need to add or
patch is vendored **here** instead, mirroring the submodule's `steam/` layout.

At generation time `compileProto` (in `../generator.go`) puts `local-protos/<subdir>`
**first** on protoc's include path, so a file here shadows the submodule's copy
of the same name. Subdirectories with no overrides (`tf2`, `dota2`, `csgo`) are
untouched.

## Files

| File | Relation to upstream |
|------|----------------------|
| `steam/steammessages_clientserver_login.proto` | **Copy of upstream + one field.** Adds `optional string access_token = 108;` to `CMsgClientLogon` — lets a bot log on with a modern refresh token instead of a password. Keep in sync with upstream if the login messages change. |
| `steam/steammessages_authentication.steamclient.proto` | **Hand-authored, minimal.** Defines only the `IAuthenticationService` request/response pairs the headless credential-login flow uses, plus `CMsgClientHello` (EMsg 9805). Field numbers / enum values match SteamKit2. Self-contained: no imports. |

## Regenerating

The committed `.pb.go` files are the source of truth — only regenerate when you
change a proto here, and always `go build ./...` afterward. Match the
protoc-gen-go version the rest of the committed protobufs were produced with
(v1.27.1-era output) to avoid spurious churn.

**login** (and every other `steam/` proto) — run the normal generator from
`generator/`; it now picks up the override automatically:

```sh
cd generator
go run generator.go proto      # regenerates the whole steam/tf2/dota2/csgo set
```

**authentication** — self-contained, so regenerate it directly (from the repo
root) rather than wiring it into the generator's file map:

```sh
protoc -I generator/local-protos/steam \
  --go_out=. --go_opt=module=github.com/sicdex/go-steam-ws \
  steammessages_authentication.steamclient.proto
# -> protocol/protobuf/unified/steammessages_authentication.steamclient.pb.go
```

The submodule stays pristine through all of this — nothing writes into `Protobufs/`.
