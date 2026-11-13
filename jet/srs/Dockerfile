ARG UBUNTU_IMAGE=ubuntu:22.04
FROM ${UBUNTU_IMAGE} AS build

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates gcc g++ make patch perl unzip curl \
        automake pkg-config tclsh cmake libssl-dev \
        libavcodec-dev libavformat-dev libavutil-dev libswresample-dev \
        libswscale-dev libavdevice-dev libavfilter-dev \
    && rm -rf /var/lib/apt/lists/*

COPY src /srs-src
WORKDIR /srs-src/trunk

# SRS 8 强制开启 RTC/SRT 编译，这里只保留 RTMP/HTTP-FLV/HLS 运行配置；
# 用系统 openssl 与系统 ffmpeg，避免额外编译第三方库（缩短首次构建时间）。
RUN ./configure --rtc=off --srt=off --ffmpeg-fit=off --https=off \
        --sys-ssl=on --sys-ffmpeg=on --jobs=4 \
    && make \
    && make install

FROM ${UBUNTU_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive TZ=Asia/Shanghai

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg curl ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /usr/local/srs /usr/local/srs
COPY srs.conf /usr/local/srs/conf/srs.conf

WORKDIR /usr/local/srs

EXPOSE 1935 1985 8080

# SRS 启动时会校验 max_connections 与文件句柄上限，先把 ulimit 抬上去
CMD ["bash", "-lc", "ulimit -n 65535 2>/dev/null || true; exec ./objs/srs -c conf/srs.conf"]
