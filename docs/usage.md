# Using parrot

## The public server

`tryparrot.site` accepts clients without a token. Point parrot at it once:

```sh
export PARROT_SERVER=tryparrot.site
```

or put `server = "tryparrot.site"` in `~/.config/parrot/config.toml` to make it the default.

```sh
parrot http 3000            # https://<random-name>.tryparrot.site
parrot tcp 5432             # tcp://tryparrot.site:<port>
parrot udp 51820            # udp://tryparrot.site:<port>
```

- Anyone with the URL can reach your tunnel. Add `--auth user:pass` to require a login on an HTTP tunnel.
- Subdomains requested with `-s` are first come, first served.
- Browsers see a short "you're visiting a site run by someone else" page the first time they open a tunnel that was opened without a token. API calls, webhooks, and `curl` are not affected, and a program can skip it by sending a `Parrot-Skip-Warning` header.

## Tunnels over SSH

Any computer with `ssh`, Windows 10 and later included, can open a tunnel without installing parrot:

```sh
ssh -p 443 -R0:localhost:3000 tryparrot.site              # HTTP tunnel with a random name
ssh -p 443 -R myapp:80:localhost:3000 tryparrot.site      # ask for myapp.tryparrot.site
ssh -p 443 -R0:localhost:3000 qr@tryparrot.site           # also show a QR code to open it on a phone
ssh -p 443 -R0:localhost:5432 tcp@tryparrot.site          # raw TCP tunnel
ssh -p 443 -R0:localhost:3000 prt_...@tryparrot.site      # with a token
```

The URL appears in your terminal, and the tunnel lasts until you press ctrl+c. Options in the username combine with `+`, as in `prt_...+qr@`. UDP needs the parrot client.

ssh doesn't reconnect after a dropped connection, so for long-lived tunnels add `-o ServerAliveInterval=30`, ask for a fixed name, or use the parrot client.

## Background tunnels

Add `-d` to `http`, `tcp`, `udp`, or `up` and a small background daemon keeps the tunnel running after you close the terminal:

```sh
parrot http 3000 -d         # prints the URL and returns
parrot ls                   # what's running, with status and uptime
parrot logs http-3000 -f    # follow one tunnel's log
parrot stop http-3000       # or: parrot stop --all
```

## parrot.toml

```toml
[tunnels]
web = 3000
db  = "tcp:5432"
vpn = "udp:51820"

[tunnels.api]
target = 8080
subdomain = "acme-api"
auth = "admin:hunter2"
host = "api.local"          # rewrite the Host header sent to your app
```

`parrot up` runs every tunnel in the file, and `parrot up api web` runs just those. Use `-c` to pick another file.

With `-d`, the file is the source of truth for its background tunnels. Run `parrot up -d` again after editing it and they catch up: new ones start, changed ones restart, and ones you removed stop. A restarted tunnel keeps its URL unless you change its subdomain or port. `parrot up -d web` updates only the tunnels you name, and `parrot down` stops everything that came from the file. Tunnels started on their own, like `parrot http 3000 -d`, are never touched.

## Starting at login

Mark tunnels with `autostart` in `~/.config/parrot/config.toml` and install the daemon as a systemd user service (Linux). At every start, the daemon runs exactly the tunnels marked `autostart`:

```toml
[tunnels.web]
target = 3000
subdomain = "myapp"
autostart = true
```

```sh
parrot service install      # starts now and at every login
parrot service status
parrot service uninstall
```

User services stop when you log out. Run `sudo loginctl enable-linger $USER` to keep them running.

## Other servers

For a [server you run](self-hosting.md) or someone else's, log in once with a token from its operator, or connect anonymously if it allows that:

```sh
parrot login prt_... --server example.com    # with a token
export PARROT_SERVER=example.com             # or anonymously
```

## Installing

The install script picks the right binary for Linux or macOS (amd64 or arm64), verifies its checksum, and installs it to `/usr/local/bin`, or `~/.local/bin` without sudo. Set `PARROT_VERSION=v0.1.9` for a specific release or `PARROT_INSTALL_DIR` to choose the location.

To build from source with Go 1.27+:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o parrot ./cmd/parrot
```
