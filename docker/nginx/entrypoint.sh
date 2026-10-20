#!/bin/sh
set -eu

# Alpine's stock nginx.conf includes http.d/*.conf INSIDE the http{} block (for
# server blocks and other http-context directives like proxy_cache_path). The
# nginx-mod-stream package adds conf.d/stream.conf which provides the stream{}
# block and does `include stream.d/*.conf;`. HTTP templates render into http.d;
# stream server blocks render into stream.d.
mkdir -p /etc/nginx/http.d /etc/nginx/http.d/cache.conf.d /etc/nginx/stream.d

# Defaults for templated values that aren't always set in the environment
# (e.g. running this image standalone, outside docker-compose). Must be
# `export`ed, not just shell-local -- envsubst runs as a separate process
# and only sees real environment variables; a shell-local `:=` default
# would render `max_size= min_free=` (and fail nginx -t) since envsubst
# substitutes an unset var with an empty string. CACHE_ROOT and
# NGINX_PURGE_SECRET are always supplied by docker-compose.yml so they have
# no default here.
export CACHE_MAX_SIZE="${CACHE_MAX_SIZE:-500g}"
export CACHE_MIN_FREE="${CACHE_MIN_FREE:-10g}"
export CACHE_INACTIVE="${CACHE_INACTIVE:-365d}"
export CACHE_KEYS_ZONE="${CACHE_KEYS_ZONE:-100m}"
export UPSTREAM_DNS="${UPSTREAM_DNS:-1.1.1.1}"

# Restrict envsubst to the explicit variable list below. nginx config is
# full of $variable syntax ($uri, $cacheidentifier, etc.) that must NOT be
# touched by envsubst -- an unrestricted call would blank those out since
# they aren't real shell env vars. Extend this list explicitly (never
# remove the restriction) when a new templated value is added.
ENVSUBST_VARS='${CACHE_ROOT} ${NGINX_PURGE_SECRET} ${CACHE_MAX_SIZE} ${CACHE_MIN_FREE} ${CACHE_INACTIVE} ${CACHE_KEYS_ZONE} ${UPSTREAM_DNS}'

for f in /etc/nginx/conf.d.templates/*.conf; do
  envsubst "$ENVSUBST_VARS" < "$f" > "/etc/nginx/http.d/$(basename "$f")"
done
for f in /etc/nginx/conf.d.templates/cache.conf.d/*.conf; do
  envsubst "$ENVSUBST_VARS" < "$f" > "/etc/nginx/http.d/cache.conf.d/$(basename "$f")"
done
# Stream-context templates: nginx-mod-stream provides stream{} in conf.d/stream.conf
# which already does `include stream.d/*.conf;` -- render server blocks there.
for f in /etc/nginx/stream.templates/*.conf; do
  envsubst "$ENVSUBST_VARS" < "$f" > "/etc/nginx/stream.d/$(basename "$f")"
done

# 40_cdn_allowlist.conf's `include /shared/nginx/40_cdn_allowlist.conf;`
# (KTD7) needs a target file to exist before nginx can even start -- on
# first boot, before the backend has written anything, that path is empty.
# Seed a deny-by-default bootstrap fragment so nginx can come up standalone
# WITHOUT acting as an open forward proxy in the window before the backend
# writes the real allow-list. A permissive default (every Host enabled) would
# let any LAN client proxy arbitrary requests (incl. internal hosts) through
# the cache during that window; deny-by-default closes that. The backend
# overwrites this exact path with the real allow-list once it is up, and
# nginx's reload picks that up the same way it picks up any later toggle.
mkdir -p /shared/nginx
if [ ! -f /shared/nginx/40_cdn_allowlist.conf ]; then
  cat > /shared/nginx/40_cdn_allowlist.conf <<'EOF'
# Bootstrap fragment seeded by entrypoint.sh: no backend-managed allow-list has
# been written yet, so every Host is denied (returns 403) rather than nginx
# proxying arbitrary hosts. Overwritten by the backend's first toggle/sync.
map $host $cdn_enabled {
    default 0;
}
EOF
fi

# Validate before ever starting nginx the first time too -- a broken
# bootstrap or template render should fail loudly rather than crash-loop
# inside the container with no diagnostic.
nginx -t

# Self-reload on change to the backend-managed allow-list fragment (KTD7):
# the backend only writes the file; nginx watches its own input and reloads
# itself, with no cross-container signaling. `nginx -s reload` re-execs
# worker processes against the new config without dropping the cache or
# active connections. If the regenerated fragment is invalid, `nginx -t`
# fails and we skip the reload, leaving the prior (validated) config
# running rather than taking nginx down.
(
  while inotifywait -e close_write -e create -e moved_to -e delete /shared/nginx >/dev/null 2>&1; do
    if nginx -t >/var/log/nginx/reload-validate.log 2>&1; then
      nginx -s reload
    else
      echo "$(date -Iseconds) allow-list reload skipped: nginx -t failed, keeping prior config" >&2
      cat /var/log/nginx/reload-validate.log >&2
    fi
  done
) &

exec nginx -g 'daemon off;'
