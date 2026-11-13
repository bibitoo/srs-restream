FROM golang:1.25 AS build

WORKDIR /src
COPY src/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/srs-control .

FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive TZ=Asia/Shanghai DATA_DIR=/data

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg curl ca-certificates tzdata jq \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/srs-control /usr/local/bin/srs-control
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh && mkdir -p /data/recordings /data/logs /data/assets /data/hls

EXPOSE 1986
VOLUME ["/data"]

HEALTHCHECK --interval=20s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS -o /dev/null http://127.0.0.1:1986/api/health || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
