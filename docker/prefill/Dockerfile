# Pinned exact version (Risks & Dependencies: undocumented integration
# contract -- steam-lancache-prefill's app-selection file path/format was
# discovered via source inspection of the pinned v2.2.2 tag, not a stable
# public API) -- bump deliberately, not via :latest, and re-verify
# /Config's layout against the new tag before doing so.
FROM tpill90/steam-lancache-prefill:v2.2.2

# inotify-tools for the trigger-file watcher (KTD7's pattern, extended to
# a longer-running job, U8's Approach); `timeout` (coreutils) is already
# present on the Ubuntu 20.04 base.
RUN apt-get update \
    && apt-get install -y --no-install-recommends inotify-tools \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Overrides the base image's ENTRYPOINT (/SteamPrefill) -- this watcher
# loop invokes /SteamPrefill itself once triggered, rather than running it
# directly as the container's main process.
ENTRYPOINT ["/entrypoint.sh"]
