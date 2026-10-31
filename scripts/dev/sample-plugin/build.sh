#!/bin/sh
# Build the sample plugin image locally and print its image id (usable as a pinned image).
set -eu
cd "$(dirname "$0")"
D=$(mktemp -d)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$D/sample-plugin" .
printf 'FROM scratch\nCOPY sample-plugin /sample-plugin\nENTRYPOINT ["/sample-plugin"]\n' > "$D/Dockerfile"
docker build -q -t forgeline-sample-plugin "$D" >/dev/null
rm -rf "$D"
docker image inspect --format '{{.Id}}' forgeline-sample-plugin
