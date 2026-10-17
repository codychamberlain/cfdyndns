# cfdyndns

A small dynamic DNS updater for Cloudflare. It checks this machine's public IP on a timer and updates one or more Cloudflare DNS records when the IP changes. It is a single static binary with no dependencies outside the Go standard library.

## How it works

1. Finds the public IP. It asks Cloudflare's `cdn-cgi/trace` first and falls back to ipify, icanhazip and ifconfig.me. It dials over IPv4 only for `A` records and IPv6 only for `AAAA` records.
2. If the IP differs from the last one it saw, it looks up each record and `PATCH`es only its `content`, so the TTL, proxy setting and comments stay as they were. A record that doesn't exist yet is created.
3. Repeats every `CF_INTERVAL` (default 5m).

## Cloudflare token

Create one at <https://dash.cloudflare.com/profile/api-tokens> with the **Edit zone DNS** template, limited to your zone. It needs `Zone:Read` and `DNS:Edit`.

## Install (systemd)

```sh
make                 # build as your user
sudo make install    # binary, unit file, /etc/cfdyndns/cfdyndns.env (mode 600)
sudo $EDITOR /etc/cfdyndns/cfdyndns.env
sudo systemctl enable --now cfdyndns
journalctl -u cfdyndns -f
```

The service runs under a throwaway `DynamicUser` with a read-only filesystem and can only open IPv4 and IPv6 sockets.

## Configuration

All settings are environment variables, which systemd reads from `/etc/cfdyndns/cfdyndns.env`:

| Variable          | Required | Default | Notes                                    |
|-------------------|----------|---------|------------------------------------------|
| `CF_API_TOKEN`    | yes      |         | API token                                |
| `CF_ZONE_NAME`    | one of   |         | e.g. `example.com`                       |
| `CF_ZONE_ID`      | one of   |         | skips the zone lookup                    |
| `CF_RECORD_NAMES` | yes      |         | comma-separated FQDNs                    |
| `CF_RECORD_TYPE`  | no       | `A`     | `A` or `AAAA`                            |
| `CF_INTERVAL`     | no       | `5m`    | Go duration, at least `30s`              |
| `CF_TTL`          | no       | `1`     | used only when creating; `1` means auto  |
| `CF_PROXIED`      | no       | `false` | used only when creating                  |

## Run manually

```sh
set -a; . ./my.env; set +a
./cfdyndns -once -debug
```

`-once` runs a single update and exits non-zero if it fails. You can use it from cron instead of running the service.

To update both IPv4 and IPv6 records, run two instances with different env files, one with `CF_RECORD_TYPE=AAAA`.

## Uninstall

```sh
sudo make uninstall
```

## License

MIT. See [LICENSE](LICENSE).
