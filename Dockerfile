FROM golang:1.27.3 AS builder

ARG TAG="nightly"
ARG COMMIT=""
# Pinned yt-dlp release and its published SHA2-256SUMS entry. The running copy
# self-updates (downloader.self_update), this is only the verified starting point.
ARG YTDLP_VERSION="2026.10.21"
ARG YTDLP_SHA256="b7af77f686e95f7845ce8d09d21957718febb8b48b30555adb9dd2292ad7bf14"

WORKDIR /build

COPY . .

RUN make build TAG="${TAG}" COMMIT="${COMMIT}"

# Download yt-dlp
RUN wget -O /usr/bin/yt-dlp "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp" && \
    echo "${YTDLP_SHA256}  /usr/bin/yt-dlp" | sha256sum -c - && \
    chmod 0755 /usr/bin/yt-dlp

# Alpine 3.22 will go EOL on 2027-05-01
FROM alpine:3.22.7

WORKDIR /app

# deno is required for yt-dlp (ref: https://github.com/yt-dlp/yt-dlp/issues/14404)
RUN apk --no-cache upgrade && \
    apk --no-cache add ca-certificates python3 py3-pip ffmpeg tzdata libc6-compat deno

# Unprivileged runtime account. yt-dlp updates itself in place, so the entrypoint
# copies the pinned binary into /run/yt-dlp (writable by this account, or a tmpfs
# when the container root filesystem is read-only) and runs it from there.
RUN addgroup -S -g 10001 podsync && \
    adduser -S -D -H -u 10001 -G podsync -h /tmp -s /sbin/nologin podsync && \
    mkdir -p /opt/yt-dlp /run/yt-dlp && \
    chown podsync:podsync /run/yt-dlp

COPY --from=builder /usr/bin/yt-dlp /opt/yt-dlp/youtube-dl
COPY --from=builder /build/bin/podsync /app/podsync
COPY --from=builder /build/html/index.html /build/html/index.js /app/html/
COPY --chmod=0755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# Caches (yt-dlp, deno) and downloads in progress go to /tmp.
ENV PATH="/run/yt-dlp:/opt/yt-dlp:${PATH}" \
    HOME=/tmp \
    XDG_CACHE_HOME=/tmp/.cache

USER 10001:10001

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["--no-banner"]
