# Alpine base, not Debian/nginx.org (KTD1): GetPageSpeed-style third-party
# cache-purge modules are strictly ABI-pinned to their exact nginx build, so
# a generic stock nginx 1.30.2 binary cannot load a third-party prebuilt
# module. Alpine's nginx and nginx-mod-http-cache-purge apk packages are
# version-matched, satisfying "stock package, no compile" while keeping an
# exact pinned version. nginx-mod-stream must match too -- all three pins
# move together.
#
# Deviation from a literal reading of KTD1/the Risks mitigation ("pin the
# base tag"): nginx=1.30.4-r3 / nginx-mod-http-cache-purge=1.30.4-r3 is only
# available paired in Alpine's `edge` branch as of this build -- no
# numbered stable release (3.21, 3.22) has cut it yet (confirmed against
# the live apk index). FROM alpine:edge is therefore the only tag carrying
# the pinned version; the exact-version apk pins below are what actually
# enforce the pairing the Risks section asks for -- if edge moves past
# 1.30.4-r3, `apk add` fails the build loudly instead of silently pulling a
# mismatched pair.
FROM alpine:edge
RUN apk add --no-cache \
      nginx=1.30.4-r3 \
      nginx-mod-http-cache-purge=1.30.4-r3 \
      nginx-mod-stream=1.30.4-r3 \
      gettext \
      inotify-tools \
    && rm -f /etc/nginx/http.d/default.conf
COPY conf.d/ /etc/nginx/conf.d.templates/
COPY stream.templates/ /etc/nginx/stream.templates/
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 80 443 8090
ENTRYPOINT ["/entrypoint.sh"]
