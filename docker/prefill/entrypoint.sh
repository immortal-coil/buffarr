#!/bin/sh
set -eu

# Confirmed against the pinned v2.2.2 source (AppConfig.cs: ConfigDir =
# Path.Combine(AppContext.BaseDirectory, "Config"); the base image's
# Dockerfile places the binary at "/") and independently against the
# upstream project's own Docker setup guide, which documents mounting a
# persistent volume at this exact path. This path is NOT configurable via
# env var on the upstream binary's side -- it's compiled in.
CONFIG_DIR=/Config
TRIGGER="$CONFIG_DIR/trigger"
STATUS="$CONFIG_DIR/status.json"
SELECTION="$CONFIG_DIR/selectedAppsToPrefill.json"

# Generous default: large libraries/slow links can legitimately take a
# long time. The real purpose of this bound is to guarantee the trigger
# lock always releases -- including when steam-lancache-prefill is stuck
# prompting for interactive login credentials it will never receive in
# this non-interactive container (an operator who hasn't completed the
# one-time interactive `docker compose run --rm --entrypoint /SteamPrefill
# prefill` login setup hits exactly this path) -- not to bound a healthy
# download to some "reasonable" guess.
MAX_RUNTIME_SECONDS="${PREFILL_MAX_RUNTIME_SECONDS:-7200}"

mkdir -p "$CONFIG_DIR"

write_status() {
  request_id="$1"; state="$2"; exit_code="$3"; error="$4"
  finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  tmp="$STATUS.tmp.$$"
  cat > "$tmp" <<EOF
{"request_id":"$request_id","state":"$state","exit_code":$exit_code,"finished_at":"$finished_at","error":"$error"}
EOF
  mv "$tmp" "$STATUS"
  # The trigger lock is released only once a terminal status has been
  # written, on every path (success, non-zero exit, or timeout) -- the
  # backend's "is a prefill already running" check is just "does this
  # file exist," so it must never be removed before a result exists to
  # poll for, and must always eventually be removed so a hung run doesn't
  # permanently block future triggers.
  #
  # Only release the lock if it's still ours: a cancelled run keeps running
  # (SteamPrefill ignores the trigger deletion), and by the time it finishes
  # the worker may have started the next queued run, writing a fresh trigger
  # with a new request ID. Blindly removing $TRIGGER here would delete the
  # next run's lock and wedge it. The trigger file holds the owning request
  # ID (see Sidecar.Trigger), so match before removing.
  [ "$(cat "$TRIGGER" 2>/dev/null)" = "$request_id" ] && rm -f "$TRIGGER"
}

run_prefill() {
  request_id="$1"

  if [ ! -s "$SELECTION" ]; then
    write_status "$request_id" "failed" "-1" "selection file missing or empty"
    return
  fi

  # --force: without it, SteamPrefill skips an app entirely whenever its own
  # successfullyDownloadedDepots.json already lists the current manifest,
  # never issuing a single request -- so nginx's cache (the actual source of
  # truth for "is this on disk") never gets a chance to serve it. --force
  # makes every run ask nginx for every file; nginx's own cache then does the
  # right thing on its own (HIT already-cached bytes, MISS and fetch only
  # what's missing/changed), exactly like a real Steam client redownload.
  exit_code=0
  timeout "${MAX_RUNTIME_SECONDS}s" /SteamPrefill prefill --force --no-ansi >"$CONFIG_DIR/last-run.log" 2>&1 || exit_code=$?

  # SteamPrefill exits 0 even when it fails to download an app's manifests or
  # content: it logs the failure, prints "Prefilled N apps totaling 0 b" with a
  # non-zero "Failed" count, and still returns 0. Keying status off the exit
  # code alone therefore reports a phantom "success" with nothing cached. When
  # the run exits 0, additionally scan its own log for the failure markers it
  # prints on a skipped/failed app. These phrases appear ONLY on a failed
  # download -- a healthy run, or one that is already up to date (legitimately
  # "0 b"), never prints them -- so this does not mis-flag a good run.
  if [ "$exit_code" -eq 0 ]; then
    if grep -qiE "Unable to download|Skipping app|Unexpected download error" "$CONFIG_DIR/last-run.log" 2>/dev/null; then
      write_status "$request_id" "failed" "$exit_code" "SteamPrefill could not download content -- nothing cached (see last-run.log)"
    else
      write_status "$request_id" "success" "$exit_code" ""
    fi
  elif [ "$exit_code" -eq 124 ]; then
    write_status "$request_id" "failed" "$exit_code" "timed out after ${MAX_RUNTIME_SECONDS}s"
  else
    write_status "$request_id" "failed" "$exit_code" "SteamPrefill exited non-zero"
  fi
}

# The trigger file's entire content is just the request-id token (not JSON)
# -- read with `cat`, no JSON parser needed in this shell script.
process_trigger() {
  request_id="$(cat "$TRIGGER" 2>/dev/null || true)"
  if [ -n "$request_id" ]; then
    run_prefill "$request_id"
  fi
}

# Process any trigger that was written while this container was not running
# (inotify only delivers future events, so an existing trigger file on
# startup would otherwise be silently dropped after a container restart).
if [ -f "$TRIGGER" ]; then
  process_trigger
fi

# Watch only moved_to: atomicWriteFile creates a temp file (close_write),
# then renames it to the final name (moved_to), so the rename is the event
# that signals a complete file.
#
# Monitor mode (-m) streams events continuously instead of exiting after the
# first one. This matters because the backend writes TWO files per trigger --
# the selection file and then the trigger file, each an atomic temp+rename
# (one moved_to event apiece). The previous single-shot
# `while changed=$(inotifywait ...)` form exited after the FIRST event
# (usually the selection) and could miss the SECOND (the trigger) in the gap
# before inotifywait restarted -- silently dropping the trigger and wedging
# IsRunning() until the next container restart. In monitor mode the trigger
# event stays queued in the pipe while run_prefill blocks and is read as soon
# as it returns, so no trigger event is ever lost to a restart gap.
inotifywait -m -e moved_to --format '%f' "$CONFIG_DIR" 2>/dev/null | while read -r changed; do
  case "$changed" in
    trigger)
      process_trigger
      ;;
  esac
done
