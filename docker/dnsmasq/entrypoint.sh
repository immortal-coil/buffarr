#!/bin/sh
set -eu

# Two backend-managed paths, two reload mechanisms (KTD3/KTD6/KTD7):
#   cdn-hosts          -- exact hostnames, dnsmasq's --addn-hosts. Confirmed
#                          against dnsmasq's own docs: SIGHUP re-reads this
#                          (and --hostsdir) but "does NOT re-read the
#                          configuration file."
#   cdn-wildcards.conf -- wildcard CDN domains (e.g. "*.cdn.ubi.com") need
#                          --address=/domain/ip, which lives in the main
#                          config and is therefore restart-only.
# Both paths must match internal/config.Config's
# DNSMasqAddnHostsPath/DNSMasqWildcardConfPath defaults exactly, and the
# `case` arms below match on their basenames.
mkdir -p /shared/dnsmasq
[ -f /shared/dnsmasq/cdn-hosts ] || : > /shared/dnsmasq/cdn-hosts
[ -f /shared/dnsmasq/cdn-wildcards.conf ] || : > /shared/dnsmasq/cdn-wildcards.conf

UPSTREAM_DNS="${UPSTREAM_DNS:-1.1.1.1}"

# This script -- not dnsmasq -- stays PID 1. Unlike nginx (U2), which can
# `exec` straight into the server and still reload itself in place via
# `-s reload`, the wildcard-domain path here needs a full process restart,
# which a script can only do for a *child* process, not for itself after
# exec'ing away. Running dnsmasq as a managed child keeps the supervisor in
# control of its lifecycle (KTD7's self-reload, restart variant).
start_dnsmasq() {
  dnsmasq --keep-in-foreground --no-resolv --server="$UPSTREAM_DNS" \
    --addn-hosts=/shared/dnsmasq/cdn-hosts \
    --conf-file=/shared/dnsmasq/cdn-wildcards.conf &
  DNSMASQ_PID=$!
}

start_dnsmasq

# Forward container shutdown (`docker stop`) to the managed child instead of
# leaving it orphaned when this script exits.
trap 'kill "$DNSMASQ_PID" 2>/dev/null; exit 0' TERM INT

# Watch only moved_to (the atomic rename that completes WriteFile) and
# delete. close_write/create are intentionally excluded: WriteFile creates a
# temp file (.dnsmasq-*.tmp), closes it (close_write), then renames it to
# the final name (moved_to). If we watched close_write, the temp-file event
# would cause inotifywait to exit and restart; the rename can then fire in
# the gap between iterations and be silently missed -- a startup-race
# observed in practice. Watching moved_to only means the first event
# inotifywait exits on is always the rename to cdn-hosts or
# cdn-wildcards.conf, with no intermediate event to create a gap.
while changed=$(inotifywait -e moved_to -e delete --format '%f' /shared/dnsmasq 2>/dev/null); do
  case "$changed" in
    cdn-hosts)
      kill -HUP "$DNSMASQ_PID" 2>/dev/null || true
      ;;
    cdn-wildcards.conf)
      kill "$DNSMASQ_PID" 2>/dev/null || true
      wait "$DNSMASQ_PID" 2>/dev/null || true
      start_dnsmasq
      ;;
  esac
done
