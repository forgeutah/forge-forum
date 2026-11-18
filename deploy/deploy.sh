#!/usr/bin/env bash
# Build forge-forum for the VM and install or upgrade it there.
#
#   FORUM_SSH="ssh forge-forum.exe.xyz" deploy/deploy.sh
#
# FORUM_SSH is the command that runs a shell command on the VM. It is
# split on spaces, so it may carry flags or a relay, for example
# FORUM_SSH="ssh exe2 ssh forge-forum".
#
# The script never overwrites /etc/forge-forum.env once it exists.
set -euo pipefail

cd "$(dirname "$0")/.."
: "${FORUM_SSH:=ssh forge-forum.exe.xyz}"
read -r -a SSH <<<"$FORUM_SSH"

remote() { "${SSH[@]}" "$1"; }
push() { "${SSH[@]}" "cat > $2" <"$1"; }

echo "==> building linux/amd64 binary"
mkdir -p bin
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/forge-forum-linux-amd64 ./cmd/forge-forum

echo "==> copying files"
push bin/forge-forum-linux-amd64 /tmp/forge-forum.new
push deploy/forge-forum.service /tmp/forge-forum.service
push deploy/forge-forum.env.example /tmp/forge-forum.env.example

echo "==> installing"
remote 'set -e
id -u forge-forum >/dev/null 2>&1 || sudo useradd --system --no-create-home --shell /usr/sbin/nologin forge-forum
sudo install -m 0755 /tmp/forge-forum.new /usr/local/bin/forge-forum
sudo install -m 0644 /tmp/forge-forum.service /etc/systemd/system/forge-forum.service
if [ ! -e /etc/forge-forum.env ]; then
  sudo install -m 0600 -o root -g root /tmp/forge-forum.env.example /etc/forge-forum.env
  echo "installed a new /etc/forge-forum.env from the example: fill it in (see deploy/RUNBOOK.md)"
fi
rm -f /tmp/forge-forum.new /tmp/forge-forum.service /tmp/forge-forum.env.example
sudo systemctl daemon-reload
sudo systemctl enable forge-forum >/dev/null 2>&1
sudo systemctl restart forge-forum
sleep 2
systemctl is-active forge-forum || { sudo journalctl -u forge-forum -n 20 --no-pager; exit 1; }
curl -fsS http://127.0.0.1:8080/healthz'

echo "==> deployed"
