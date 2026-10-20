# Buffarr Install Runbook

A step-by-step procedure for installing Buffarr from scratch, verifying each
stage, and recovering from the common failure modes. For *what* Buffarr is and
how it works conceptually, see [README.md](../README.md). This document is the
*how-to-install* companion.

The whole install is roughly: configure `.env`, bring up the stack, finish Steam
setup in the browser, authenticate the prefill sidecar once, point LAN DNS at
the server, then verify a real download caches.

Buffarr is designed to run directly on a Linux host via the command line. It
binds port 53 for DNS interception, manages Docker containers directly, and
expects shell access for first-run setup. Running it through a container
management UI adds a layer between Buffarr and the host that tends to get in
the way.

---

## 0. Prerequisites

Work through this checklist before starting. A miss here is the cause of most
failed installs.

- [ ] A Linux host with **Docker** and the **Compose plugin**, verified with `docker compose version`.
- [ ] **git**, used at step 1, and **openssl**, used at step 2 for secret generation.
- [ ] These host ports free: **53/tcp**, **53/udp**, **80**, **443**, **9080**, **9443**.
      The usual conflict is `systemd-resolved` already holding **:53**; see Troubleshooting.
- [ ] A disk location for cache storage, such as a dataset, LVM volume, or plain directory,
      with room for your intended `CACHE_MAX_SIZE`.
- [ ] The ability to point LAN clients' **DNS** at this host, via router DHCP, Pi-hole, or similar.
- [ ] A **Steam Web API key** from https://steamcommunity.com/dev/apikey and your
      account's **SteamID64** from https://steamid.io.
- [ ] `sudo`, if your cache lives on a bind-mounted host path, deployment mode B.

---

## Hardware and sizing

Before you size the disk and fill in `.env`, see the **[Sizing Guide](SIZING.md)**:
hardware, meaning NIC, disk, and RAM ranked by impact, and how to pick `CACHE_MAX_SIZE`,
`CACHE_MIN_FREE`, and `CACHE_KEYS_ZONE`.

---

## 1. Get the code

```bash
git clone https://<your-git-host>/buffarr.git
cd buffarr
```

---

## 2. Configure `.env`

```bash
cp .env.example .env
```

Generate the two secrets and edit the file:

```bash
openssl rand -hex 32   # use for SESSION_SECRET
openssl rand -hex 32   # use for NGINX_PURGE_SECRET
$EDITOR .env
```

| Key | Required? | Notes |
|---|---|---|
| `BUFFARR_IP` | **Yes** | This host's LAN IP. The address CDN hostnames resolve to for clients. Cannot be auto-detected. |
| `CACHE_ROOT` | **Yes** | Container-internal cache path. In mode B, match it to the host mount. |
| `CACHE_MAX_SIZE` | **Yes** | e.g. `500g`. nginx disk-safety ceiling, sized to your disk. |
| `CACHE_MIN_FREE` | **Yes** | e.g. `100g`. Reserve so nginx never fills the disk. On multi-TB caches `10g` is too thin — see the [Sizing Guide](SIZING.md). |
| `CACHE_INACTIVE` | Default `365d` | nginx time-based eviction, meaning idle-slice removal. Raise it, for example to `3650d`, to effectively disable it. |
| `CACHE_KEYS_ZONE` | Default `100m` | RAM size of nginx's cache key index. Effective cache cap: `100m` ≈ 800 GB. Size to your disk, about `470m` per 3.7 TB. |
| `SESSION_SECRET` | **Yes** | `openssl rand -hex 32`. |
| `NGINX_PURGE_SECRET` | **Yes** | `openssl rand -hex 32`. Shared secret for the backend's PURGE calls to nginx. |
| `STEAM_WEB_API_KEY` | **Yes** | From the Steam dev page. May instead be entered in the first-run setup form. |
| `STEAM_OPERATOR_ID64` | Optional | The only account allowed to log in. Set here, or via the setup form. |
| `UPSTREAM_DNS` | Default `1.1.1.1` | Where non-intercepted DNS queries are forwarded. Must **not** be a resolver that redirects CDN domains back to `BUFFARR_IP`, such as a Pi-hole configured for Buffarr interception. nginx's SNI passthrough would resolve CDN hostnames to itself and loop. Use a real upstream such as `1.1.1.1` or `8.8.8.8`. |
| `PREFILL_MAX_RUNTIME_SECONDS` | Default `7200` | Hard kill for a single preload. |
| `TZ` | Default `UTC` | Container timezone for timestamps, e.g. `America/Los_Angeles`. |
| `TLS_CERT_PATH` | Optional | Path **inside the backend container** to a custom TLS certificate in PEM form. The cert must be reachable via a volume mount. Defaults to the self-signed cert generated at first boot. |
| `TLS_KEY_PATH` | Optional | Matching private key for `TLS_CERT_PATH`. |

### Securing secrets

`SESSION_SECRET`, `NGINX_PURGE_SECRET`, and `STEAM_WEB_API_KEY` are credentials.
Handle them accordingly:

- **Never commit `.env`**, which is gitignored, and never paste secret values into
  shared docs, issues, chat, or shell history. Keep `.env` mode `600` with
  `chmod 600 .env`.
- **The Steam Web API key does not have to live in `.env`.** Either enter it in
  the **first-run setup form**, where it's stored in the catalog DB rather than the
  container's environment, or keep it in a protected file and inject it at
  deploy time rather than hard-coding it:
  ```bash
  chmod 600 ~/.secrets/steam-web-api.key
  export STEAM_WEB_API_KEY="$(cat ~/.secrets/steam-web-api.key)"
  docker compose up -d            # with sudo, use `sudo -E` so the export survives
  ```
- **Rotate on exposure.** The Steam key is revocable at
  [steamcommunity.com/dev/apikey](https://steamcommunity.com/dev/apikey). Regenerate `SESSION_SECRET` and
  `NGINX_PURGE_SECRET` with `openssl rand -hex 32`. Changing `SESSION_SECRET`
  logs everyone out. `NGINX_PURGE_SECRET` must change for both the backend and
  nginx, so restart the stack after editing it.

---

## 3. Choose a deployment mode

**Mode A. Docker-managed volume, the simplest.** Cache lives in a Docker volume.
Use plain `docker compose` and skip the target overlay.

**Mode B. Bind-mounted cache on a host path, the target host.** Cache lives on a
host directory such as a ZFS dataset. Layer in `docker-compose.target.yml` and
use **absolute paths**, because `sudo docker compose` runs from root's working
directory, not yours. The canonical invocation is:

```bash
sudo docker compose \
  -f /absolute/path/to/buffarr/docker-compose.yml \
  -f /absolute/path/to/buffarr/docker-compose.target.yml \
  --env-file /absolute/path/to/buffarr/.env \
  --project-directory /absolute/path/to/buffarr \
  <subcommand>
```

Throughout the rest of this runbook, `docker compose <args>` is shorthand. In
mode B, substitute the full invocation above.

---

## 4. Bring the stack up

```bash
docker compose up -d --build
```

`--build` compiles the images from source on first launch, which takes a few
minutes. On first boot the backend writes a self-signed TLS cert to `/data/tls/`.

Expected: four containers start — `backend`, `nginx`, `dnsmasq`, and `prefill` — all reporting `healthy`.

---

## 5. Verify the stack

```bash
docker compose ps
```

```bash
# Backend answers and redirects an un-set-up instance to /setup. Plain HTTP is
# published on the host loopback only, so test it from the host itself; LAN
# access is over HTTPS :9443.
curl -sk -o /dev/null -w '%{http_code} %{redirect_url}\n' http://127.0.0.1:9080/

# nginx cache health:
curl -s -o /dev/null -w '%{http_code}\n' http://<BUFFARR_IP>:80/healthz
```

Confirm **nginx** is `healthy` and that port **:443** is listed as published. nginx handles both HTTP caching on `:80` and HTTPS SNI passthrough on `:443`. The `:443` stream listener is required for Steam to fetch depot manifests and is the single most confusing failure if absent; see Troubleshooting.

---

## 6. First-run setup: Steam configuration

Until Steam is configured, every page redirects to `/setup`.

1. Browse to `https://<BUFFARR_IP>:9443`; the cert is self-signed, so accept the
   browser warning.
2. On the **Setup** form, enter your **Steam Web API key** and **operator
   SteamID64**. These are written to the catalog DB, so you don't have to put
   them in `.env`.
3. After saving, sign in via **Steam** OpenID. Only the operator SteamID64 is
   accepted; any other account is rejected before a session is created.

---

## 7. One-time prefill authentication

The prefill sidecar needs its **own** interactive Steam login, separate from the
console login. Do this once before your first preload:

```bash
docker compose run --rm --entrypoint /SteamPrefill prefill
```

Follow the prompts for username, password, and Steam Guard. Credentials persist
in the `prefill-shared` volume, mounted at `/Config`, until Steam invalidates the
session. Skipping this doesn't hang anything: preloads just fail after
`PREFILL_MAX_RUNTIME_SECONDS` and surface a failure in the UI.

---

## 8. Point LAN clients at the cache

Set client **DNS** to `BUFFARR_IP`:

- **Router DHCP:** set the DNS field to `BUFFARR_IP`; all clients inherit it.
- **Pi-hole / AdGuard Home:** add `BUFFARR_IP` as upstream, or conditional-forward
  the CDN domains.
- **Per client:** set the DNS server manually.

Then in the console go to **Settings → Platforms** and enable the platforms you
want intercepted, Steam first. Only enabled platforms' CDN hostnames are
redirected; everything else is forwarded to `UPSTREAM_DNS` untouched.

> Clients must use Buffarr for **DNS**, not merely route HTTP through it. DNS
> interception is what steers downloads into the cache.

### Using your own DNS server

If you already run a DNS server (Pi-hole, AdGuard Home, Unbound, a router, or
your own dnsmasq), you do not have to replace it. Buffarr's dnsmasq only needs to
answer the **CDN domains**; let your existing resolver handle everything else.
Three ways to wire it:

- **Make Buffarr the upstream, the simplest option.** Point your DNS server's
  upstream at `BUFFARR_IP`. Every query flows through Buffarr's dnsmasq, which
  redirects enabled CDN hostnames to the cache and forwards the rest to
  `UPSTREAM_DNS`. There is no domain list to maintain on your side, and your own
  filtering such as Pi-hole blocklists still applies before the query is
  forwarded. The trade-off
  is that all of your DNS passes through the cache box.
- **Conditional-forward only the CDN domains.** Forward just the lancache CDN
  domains to `BUFFARR_IP` and keep everything else on your own resolver, so only
  game-CDN lookups touch Buffarr. Pi-hole, AdGuard, Unbound, and dnsmasq all
  support conditional forwarding; you supply the CDN parent-domain list on your
  side.
- **Redirect the domains in your own resolver, with no Buffarr DNS at all.** Skip
  Buffarr's dnsmasq entirely and point the CDN domains straight at the cache. In
  Unbound, one redirect pair per domain:

  ```
  server:
      local-zone: "steamcontent.com." redirect
      local-data: "steamcontent.com. 600 IN A <BUFFARR_IP>"
      local-zone: "steamcdn-a.akamaihd.net." redirect
      local-data: "steamcdn-a.akamaihd.net. 600 IN A <BUFFARR_IP>"
      local-zone: "download.epicgames.com." redirect
      local-data: "download.epicgames.com. 600 IN A <BUFFARR_IP>"
      local-zone: "dist.blizzard.com." redirect
      local-data: "dist.blizzard.com. 600 IN A <BUFFARR_IP>"
      # ... one pair per CDN domain you want cached ...
  ```

  Pi-hole and dnsmasq have equivalents, such as a host override or an `address=`
  line per domain. Simple and fully under your control. The trade-off is that you own
  the domain list: it does not track Buffarr's bundled list or the
  **Settings -> Platforms** toggles, so you extend it yourself as CDNs change.

> **Whichever approach you use, enable the matching platforms in Settings ->
> Platforms.** nginx independently returns 403 for any Host not in its enabled
> allow-list, so a domain you redirect to the cache is only served once its
> platform is turned on in Buffarr. DNS steers the traffic in; the platform
> toggle is what lets nginx actually cache it.

To change only where Buffarr forwards non-CDN queries, set `UPSTREAM_DNS` in
`.env` to your own resolver instead of `1.1.1.1`.

---

## 9. Verify caching end to end

Either preload a small owned game from the **Preload** page or start a download on a
client whose DNS points here, then confirm:

```bash
# The game appears in the catalog with a non-zero size:
#   open the Library page, or
docker compose logs --tail=20 backend     # ingest activity

# nginx is logging cache HIT/MISS for CDN hosts:
docker compose logs --tail=20 nginx

# Disk is actually being used (mode B host path, or the Docker volume):
du -sh <CACHE_ROOT>/cache
```

A second client downloading the same game should now pull it from cache at LAN
speed. Success.

---

## 10. Optional: running behind a reverse proxy

To expose the console on a public hostname, put your own nginx in front of it and
proxy to the backend's **HTTPS** port `:9443`. A minimal site config:

```nginx
server {
    set $backend https://<backend-ip>:9443;
    server_name buffarr.example.com;
    listen 443 ssl;
    http2 on;

    ssl_certificate     /path/to/fullchain.pem;        # your public TLS cert
    ssl_certificate_key /path/to/privkey.pem;

    location / {
        proxy_pass $backend;
        proxy_ssl_verify off;                           # backend cert is self-signed
        proxy_set_header Host $host;                     # keeps Steam return_to correct
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Notes:

- **Proxy to `:9443`, not `:9080`.** The backend decides whether a request is
  secure from the actual TLS connection, not from `X-Forwarded-Proto`. Pointing
  the proxy at the plain `:9080` port makes it treat sessions as insecure and can
  break Steam OpenID sign-in.
- **`proxy_ssl_verify off`** because the backend serves a self-signed certificate.
- **`proxy_set_header Host $host`** preserves the public hostname so Steam OpenID
  builds the correct `return_to` URL.
- Assigning the upstream to a variable with `set $backend ...` lets nginx start even
  if the backend host can't be resolved at that moment.

---

## Health checks and handy commands

```bash
docker compose ps                       # health of every container
docker compose logs -f backend          # follow backend (ingest, evictions, PURGE)
docker compose logs -f nginx            # cache HIT/MISS
docker stats --no-stream                # live RAM/CPU per container
du -sh <CACHE_ROOT>/cache               # real on-disk cache size
```

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Preload reports **success** but the Library stays empty / 0 bytes | nginx's HTTPS SNI passthrough on `:443` is not working. Steam fetches depot *manifests* over HTTPS. Without the passthrough, the run caches ~0 bytes and exits cleanly. | `docker compose ps` — confirm nginx is `healthy` and `:443` is published. If not, check `docker compose logs nginx`. This is the #1 gotcha. |
| Client downloads aren't intercepted, with no cache activity | Client DNS isn't pointed at `BUFFARR_IP`, or the platform is disabled in Settings. | Fix client DNS; enable the platform. Verify with `nslookup <a-steam-cdn-host> <BUFFARR_IP>`, which should return `BUFFARR_IP`. |
| `docker compose up` fails: **port is already allocated** on :53 | `systemd-resolved`, or another resolver, holds :53. | Disable the stub listener: set `DNSStubListener=no` in `/etc/systemd/resolved.conf`, then `systemctl restart systemd-resolved`. |
| Browser warns the TLS cert is untrusted | Expected: the first-boot cert is self-signed. | Accept it, or supply your own cert/key: set `TLS_CERT_PATH` and `TLS_KEY_PATH` in `.env` to paths **inside the backend container**, and ensure the cert files are reachable via a volume mount in `docker-compose.yml`. |
| You're logged out after every update | Sessions are in-memory and don't survive a backend restart. | Expected. Log back in. Plan for one logout per backend redeploy. |
| Preloads always fail after the timeout | The prefill sidecar isn't authenticated, see step 7, or nginx's `:443` passthrough is not working. | Re-run step 7; confirm nginx is `healthy` and `:443` is published with `docker compose ps`. |
| Setup form keeps reappearing | Steam Web API key / operator SteamID64 not yet saved. | Complete step 6; the redirect clears once both are stored. |

---

## Updating

Buffarr does not publish pre-built images. Every host builds from source using
the checked-out code, so you can review what you are running before the build
and there is no upstream image to trust.

```bash
git pull
docker compose up -d --build            # rebuild changed images, recreate
```

To redeploy only the backend after a code change (faster, and the only container
that needs it for a UI/logic change):

```bash
docker compose up -d --build --no-deps backend
```

Each backend restart logs operators out, since sessions are in-memory. Expect
exactly one logout per backend redeploy.

---

## Rollback

For a source build, roll back to a known-good commit and rebuild:

```bash
git checkout <previous-good-commit>
docker compose up -d --build --no-deps backend
```

If you deploy from a registry instead of building on the host, redeploy the
previously published image tag rather than rebuilding.

---

## Teardown

```bash
docker compose down                     # stop and remove containers (keeps volumes)
docker compose down -v                  # also remove volumes (DELETES the catalog,
                                        # prefill credentials, and a Docker-managed cache)
```

In mode B the cache on the host bind-mount is not touched by `down -v`; remove
that directory manually if you want the cached game data gone too.
