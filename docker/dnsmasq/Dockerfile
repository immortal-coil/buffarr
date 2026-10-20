# --no-resolv --server=<one upstream> (entrypoint.sh) keeps this dnsmasq
# from amplifying arbitrary recursive queries the way a misconfigured open
# resolver would. Whether port 53 is reachable from beyond the operator's
# own LAN is a network/firewall-level fact about where this container's
# published port ends up, not something enforceable inside the container --
# Docker's port-publish model has no source-subnet restriction at this
# layer. Documented here rather than silently assumed: operators exposing
# this stack's host directly to the internet must firewall 53 themselves.
FROM alpine:3.20
RUN apk add --no-cache dnsmasq inotify-tools
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 53/tcp 53/udp
ENTRYPOINT ["/entrypoint.sh"]
