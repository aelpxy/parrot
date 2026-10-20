# Running your own server

You need a domain whose apex and wildcard (`example.com` and `*.example.com`) point at your server, with TCP ports `80`, `443`, and `7000` open, plus the port range for TCP and UDP tunnels (`30000-40000` by default).

## With Docker

Images for `linux/amd64` and `linux/arm64` are published to `ghcr.io/aelpxy/parrot`: `latest` and version tags for releases, `edge` for `main`. Certificates and tokens live in the `/data` volume.

1. Get a wildcard certificate. parrot prints two TXT records to add at your DNS provider and waits until they are live:

   ```sh
   docker run --rm -it -v parrot-data:/data ghcr.io/aelpxy/parrot server cert --domain example.com
   ```

2. Start the server. Keep the published range small, since Docker runs a proxy process for every published port:

   ```sh
   docker run -d --name parrot --restart unless-stopped \
     -v parrot-data:/data \
     -p 80:80 -p 443:443 -p 7000:7000 \
     -p 30000-30100:30000-30100 -p 30000-30100:30000-30100/udp \
     ghcr.io/aelpxy/parrot server --domain example.com --tcp-ports 30000-30100
   ```

3. Create a token for each client:

   ```sh
   docker exec parrot parrot server token create --name laptop
   ```

## With systemd

```sh
sudo install -m 755 parrot /usr/local/bin/parrot
sudo parrot server cert --domain example.com
sudo parrot service install --server -- --domain example.com
sudo parrot server token create --name laptop
```

`sudo parrot service status --server` shows whether it is running, and `journalctl -u parrot-server -f` follows its logs.

## Config file

Every server flag can live in a file instead, with dashes turned into underscores. Unknown keys are errors, and flags given on the command line still win:

```toml
# /etc/parrot/server.toml
domain = "example.com"
tcp_ports = "30000-40000"
allow_anonymous = true
ssh_on_https = true
```

```sh
sudo parrot service install --server -- --config /etc/parrot/server.toml
```

After editing the file, run `sudo systemctl restart parrot-server`. The `token` and `cert` commands read `data_dir` from the same file when given `--config`. Tokens stay out of it: they are secrets, kept hashed and managed with `parrot server token`.

## Options

- `--allow-anonymous` lets clients connect without a token. Browsers then see a warning page before tunnels opened without one; `--no-warning` turns it off.
- `--ssh-on-https` accepts tunnels from plain `ssh -R` on the HTTPS port, telling SSH and HTTPS apart by their first bytes. `--ssh :2222` uses a dedicated port instead. The SSH host key lives in the data directory, and its fingerprint is logged at startup.
- `--tcp-ports` sets the range for TCP and UDP tunnels.

`parrot server --help` lists everything.

## Renewing certificates

Let's Encrypt certificates last 90 days. Rerun `parrot server cert` about every 60 days. The server picks up the new certificate without a restart, and logs a warning when expiry is three weeks away.

## Development

```sh
go test -race ./...
golangci-lint run ./...
```
