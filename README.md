# parrot

Expose local services to the internet over HTTPS, TCP, or UDP.

```console
$ parrot http 3000
 parrot   ● online

  http-3000  https://brave-otter-k3j9x2mq.tryparrot.site  → localhost:3000
```

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/aelpxy/parrot/main/install.sh | sh
```

Linux and macOS, amd64 and arm64. Binaries are also on the [releases page](../../releases).

## Try it

A public server runs at `tryparrot.site`, no account needed:

```sh
export PARROT_SERVER=tryparrot.site
parrot http 3000
```

No install? Any computer with `ssh` works too:

```sh
ssh -p 443 -R0:localhost:3000 tryparrot.site
```

Anyone with the URL can reach your tunnel, so don't expose anything you wouldn't put on the open internet.

## Use

```sh
parrot                      # pick a local port interactively
parrot http 3000            # expose an HTTP app
parrot http 3000 -s myapp   # with a fixed subdomain
parrot tcp 5432             # a raw TCP service
parrot udp 51820            # a UDP service
parrot http 3000 -d         # run in the background
parrot ls                   # list background tunnels
parrot logs http-3000 -f    # follow a tunnel's log
parrot stop --all           # stop background tunnels
parrot speedtest            # measure the connection to the server
```

Tunnels you use often can live in a `parrot.toml`:

```toml
[tunnels]
web = 3000
db  = "tcp:5432"
```

`parrot up` runs them. `parrot up -d` runs them in the background and, after you edit the file, brings the running tunnels in line with it. `parrot down` stops them.

## More

- [Using parrot](docs/usage.md): the public server, SSH tunnels, background tunnels, starting at login, and other servers.
- [Running your own server](docs/self-hosting.md): Docker, systemd, config files, and certificates.
- `parrot --help` and `parrot <command> --help` cover every flag.

## License

parrot is released under the [MIT License](LICENSE).
