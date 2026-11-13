# 以 SRS 为底座的流媒体容器：SRS + 转推/混流控制面
#
#   构建：先执行 scripts/prepare-srs-source.sh 准备好 SRS 源码快照，再 docker build
#   运行：docker run -p 1935:1935 -p 8080:8080 -p 1985:1985 -p 1986:1986 \
#            -v $PWD/data:/data srs-restream:latest
#
# 镜像内三个进程：SRS 本体、srs-control 控制面、以及控制面拉起的 ffmpeg（混合/录屏/转推中继）

ARG UBUNTU_IMAGE=ubuntu:22.04

############################################################
# 1) 编译 SRS 本体
############################################################
FROM ${UBUNTU_IMAGE} AS srs-build

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates gcc g++ make patch perl unzip curl \
        automake pkg-config tclsh cmake libssl-dev \
        libavcodec-dev libavformat-dev libavutil-dev libswresample-dev libswscale-dev libavdevice-dev libavfilter-dev \
    && rm -rf /var/lib/apt/lists/*

COPY build/srs-src /srs-src
WORKDIR /srs-src/trunk

# 默认关闭 WebRTC/SRT/HTTPS：容器以 RTMP + HTTP-FLV + HLS 为主，减小体积与构建时间。
# 需要 WebRTC 时可改为：
#   --rtc=on --ffmpeg-fit=on --srt=on
ARG SRS_CONFIGURE_ARGS="--rtc=off --srt=off --ffmpeg-fit=off --https=off --sys-ssl=on --sys-ffmpeg=on --jobs=16"

RUN ./configure ${SRS_CONFIGURE_ARGS} \
    && make \
    && make install

############################################################
# 2) 编译控制面（Go，无第三方依赖）
############################################################
FROM golang:1.25 AS ctl-build

WORKDIR /src
COPY control/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/srs-control .

############################################################
# 3) 运行镜像
############################################################
FROM ${UBUNTU_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive \
    TZ=Asia/Shanghai \
    DATA_DIR=/data

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg curl ca-certificates tzdata jq \
    && rm -rf /var/lib/apt/lists/* \
    && ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone

COPY --from=srs-build /usr/local/srs /usr/local/srs
COPY --from=ctl-build /out/srs-control /usr/local/bin/srs-control
COPY docker/srs.conf /usr/local/srs/conf/srs.conf
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /data/recordings /data/logs /data/assets

# 1935 RTMP 推流/拉流；1985 SRS HTTP API；1986 控制台/业务 API；8080 HTTP-FLV + HLS + 播放器
EXPOSE 1935 1985 1986 8080

VOLUME ["/data"]

HEALTHCHECK --interval=20s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -fsS -o /dev/null http://127.0.0.1:1986/api/health || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
