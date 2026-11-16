# file-receiver

A single-binary, self-contained Go server that receives file uploads from a
phone's browser over the local network — no cloud, no account, files stay on
your machine.

## Build / Run
```bash
go build -o file-receiver .
./file-receiver
```

Open `http://<IP>:8080` from a phone on the same Wi-Fi, tap the drop zone, and
uploaded files appear in `~/Downloads/mobile_uploads` with `0600` permissions.

## Install (`~/.local/bin`)
```bash
go install github.com/ghareep/file-receiver@latest
# or, from a checkout:
./install.sh
# export PATH="$HOME/.local/bin:$PATH" if needed
```

## Flags
| Flag | Default | Description |
|---|---|---|
| `-addr` | `:8080` | Listen address (`host:port` or `:port`). Binds all interfaces; use `127.0.0.1:8080` for local-only testing. |
| `-dir` | `~/Downloads/mobile_uploads` | Upload directory. Created `0700`. |
| `-max-mb` | `500` | Maximum accepted upload size per file, in MiB. |
| `-token` | *(unset)* | If set, clients must send it in the `X-Upload-Token` header (constant-time compared). |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error`. |

## Security
Designed for a **trusted LAN**. Traffic is cleartext HTTP. On any network you
do not control, start with `-token "$(head -c 32 /dev/urandom | base64)"` and
pass the same token in `X-Upload-Token`. Uploads never overwrite an existing
file: duplicate names get `-1`, `-2`, ... appended before the extension, and
writes are atomic (temp file + fsync + rename), so a crash never leaves a
half-written file.

## Requirements
- Go 1.22+
- Linux (Debian); any platform Go supports
- `qrencode` optional, to print a QR code of the upload URL

## Development
```bash
make            # vet + race tests + build
go test -race ./...
```

## Usage
```bash
./file-receiver                                   # :8080
./file-receiver -addr 127.0.0.1:9000              # local only
./file-receiver -dir /srv/uploads -max-mb 2048 -token s3cret
```
Stop with `Ctrl-C` or `kill <pid>` — in-flight transfers are drained before exit (15s grace).
