#!/bin/sh
# file-receiver installer: builds the Go binary into ~/.local/bin.
# Go needs no system packages; qrencode is optional, for the QR-code hint.
set -eu

BINARY="${BINARY:-file-receiver}"
DEST="${DEST:-$HOME/.local/bin}"

mkdir -p "$DEST"
go build -trimpath -o "$DEST/$BINARY" .

echo "Installed: $DEST/$BINARY"
echo "Run:       $DEST/$BINARY --help"
echo "Optional:  install qrencode to print a QR code of the upload URL."
