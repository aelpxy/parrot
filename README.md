# parrot

Expose local services to the internet over HTTP(S) or raw TCP.

```console
$ parrot http 3000
 parrot   ● online

  http-3000  https://brave-otter-k3j9x2mq.tryparrot.site  → localhost:3000
```

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/aelpxy/parrot/main/install.sh | sh
```

The script picks the right binary for Linux or macOS (amd64 or arm64), verifies its checksum, and installs it to `/usr/local/bin`, or `~/.local/bin` without sudo. Set `PARROT_VERSION=v0.1.0` for a specific release or `PARROT_INSTALL_DIR` to choose the location.

You can also download a binary from the [releases page](../../releases), or build from source with Go 1.27+:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o parrot ./cmd/parrot
```

## Use

### Public server

A public parrot server runs at `tryparrot.site` and accepts clients without a token, so you can try parrot without running your own:

```sh
export PARROT_SERVER=tryparrot.site
parrot http 3000            # https://<random-name>.tryparrot.site
parrot tcp 5432             # tcp://tryparrot.site:<port>
```

To use it by default, put `server = "tryparrot.site"` in `~/.config/parrot/config.toml` instead of exporting the variable.

Anyone with the URL can reach your tunnel. Add `--auth user:pass` to require a login on HTTP tunnels, and don't expose anything you wouldn't put on the open internet. Subdomains requested with `-s` are first come, first served.

Because tunnels opened without a token could be anyone's, browsers see a short "you're visiting a site run by someone else" page the first time they open one. API calls, webhooks, and `curl` are not affected, and a program can skip the page by sending a `Parrot-Skip-Warning` header. Tunnels opened with a token never show it.

### Without installing anything

Any computer with `ssh` can open a tunnel on the public server, Windows 10 and later included:

```sh
ssh -p 443 -R0:localhost:3000 tryparrot.site              # HTTP tunnel with a random name
ssh -p 443 -R myapp:80:localhost:3000 tryparrot.site      # ask for myapp.tryparrot.site
ssh -p 443 -R0:localhost:3000 qr@tryparrot.site           # also show a QR code
ssh -p 443 -R0:localhost:5432 tcp@tryparrot.site          # raw TCP tunnel
ssh -p 443 -R0:localhost:3000 prt_...@tryparrot.site      # with a token
```

The URL appears in your terminal, and the tunnel lasts until you press ctrl+c. Options in the username combine with `+`, as in `prt_...+qr@`. ssh doesn't reconnect after a dropped connection, so for long-lived tunnels add `-o ServerAliveInterval=30`, ask for a fixed name, or use the parrot client.

### Other servers

For a [server you run](#run-a-server) or someone else's, log in once with a token from its operator, or connect anonymously if it allows that:

```sh
parrot login prt_... --server example.com    # with a token
export PARROT_SERVER=example.com             # or anonymously
```

### Commands

```sh
parrot                      # pick a listening local port interactively
parrot http 3000            # expose an HTTP app
parrot http 3000 -s myapp   # with a fixed subdomain
parrot tcp 5432             # expose a raw TCP service
parrot http 3000 -d         # run in the background
parrot ls                   # list background tunnels
parrot logs http-3000 -f    # follow a tunnel's log
parrot stop --all           # stop background tunnels
parrot speedtest            # measure latency and throughput to the server
```

Tunnels you start often can live in a `parrot.toml` and run with `parrot up`:

```toml
[tunnels]
web = 3000
db  = "tcp:5432"
```

### Background tunnels

Add `-d` to `http`, `tcp`, or `up` and parrot hands the tunnel to a small background daemon, which starts on its own and keeps it running after you close the terminal:

```sh
parrot http 3000 -d         # prints the URL and returns
parrot up -d                # every tunnel in parrot.toml
parrot ls                   # what's running, with status and uptime
parrot logs http-3000 -f    # follow one tunnel's log
parrot stop http-3000       # or: parrot stop --all
```

To bring tunnels back after a reboot, mark them in `~/.config/parrot/config.toml` and install the daemon as a systemd user service (Linux):

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

Run `parrot --help` or `parrot <command> --help` for everything else.

## Run a server

You need a domain whose apex and wildcard (`example.com` and `*.example.com`) point at your server, with these TCP ports open: `80`, `443`, `7000`, and the range used for TCP tunnels.

### With Docker

Images for `linux/amd64` and `linux/arm64` are published to `ghcr.io/aelpxy/parrot`: `latest` and version tags for releases, `edge` for `main`. Data (certificates and tokens) lives in the `/data` volume.

1. Get a wildcard certificate. parrot prints two TXT records to add at your DNS provider and waits until they are live:

   ```sh
   docker run --rm -it -v parrot-data:/data ghcr.io/aelpxy/parrot server cert --domain example.com
   ```

2. Start the server:

   ```sh
   docker run -d --name parrot --restart unless-stopped \
     -v parrot-data:/data \
     -p 80:80 -p 443:443 -p 7000:7000 -p 30000-30100:30000-30100 \
     ghcr.io/aelpxy/parrot server --domain example.com --tcp-ports 30000-30100
   ```

   Keep the published TCP range small: Docker runs a proxy process for every published port.

3. Create a token for each client:

   ```sh
   docker exec parrot parrot server token create --name laptop
   ```

Add `--allow-anonymous` to the server command to let clients connect without a token. Browser visitors of those tunnels see a warning page first; add `--no-warning` to turn it off.

To let people open tunnels with plain `ssh -R`, add `--ssh-on-https` to accept SSH on the HTTPS port (parrot tells the two apart by their first bytes), or `--ssh :2222` for a dedicated port. The server keeps its SSH host key in the data directory and logs its fingerprint at startup, so you can publish it for users to check.

### With systemd

```sh
sudo install -m 755 parrot /usr/local/bin/parrot
sudo parrot server cert --domain example.com
sudo parrot service install --server -- --domain example.com
sudo parrot server token create --name laptop
```

`sudo parrot service status --server` shows whether it is running, and `journalctl -u parrot-server -f` follows its logs.

### Renewing certificates

Let's Encrypt certificates last 90 days. Rerun the `server cert` command about every 60 days. The server picks up the new certificate without a restart and logs a warning when expiry is three weeks away.

## Development

```sh
go test -race ./...
golangci-lint run ./...
```

## License

parrot is released under the [MIT License](LICENSE).
