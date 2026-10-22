#!/bin/sh
set -e

# yt-dlp self-updates by rewriting its own file, which needs a writable location.
# Seed it from the image's pinned copy on every start; if /run/yt-dlp isn't
# writable, PATH falls back to the read-only /opt/yt-dlp copy.
if cp /opt/yt-dlp/youtube-dl /run/yt-dlp/youtube-dl 2>/dev/null; then
    chmod 0755 /run/yt-dlp/youtube-dl
else
    echo "warning: /run/yt-dlp is not writable, yt-dlp self-update will fail" >&2
fi

exec /app/podsync "$@"
