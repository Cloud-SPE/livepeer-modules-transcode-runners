ARG REGISTRY=localbuild
ARG TAG=v1.3.0
ARG UBUNTU_VERSION=24.04
ARG CODECS_IMAGE=${REGISTRY}/codecs-builder:${TAG}

FROM ${CODECS_IMAGE} AS codecs-builder

FROM ubuntu:${UBUNTU_VERSION} AS ffmpeg-builder

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        git \
        libdrm-dev \
        libssl-dev \
        libva-dev \
        libx265-dev \
        mesa-va-drivers \
        nasm \
        pkg-config \
        yasm \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY --from=codecs-builder /usr/local /usr/local
RUN ldconfig

WORKDIR /build

ENV PKG_CONFIG_PATH=/usr/local/lib/pkgconfig:/usr/local/lib/x86_64-linux-gnu/pkgconfig
ENV LD_LIBRARY_PATH=/usr/local/lib:/usr/local/lib/x86_64-linux-gnu

RUN git clone --depth 1 --branch n7.1.3 https://github.com/FFmpeg/FFmpeg.git \
    && cd FFmpeg \
    && ./configure \
        --prefix=/usr/local \
        --disable-doc \
        --disable-static \
        --enable-gpl \
        --enable-nonfree \
        --enable-vaapi \
        --enable-openssl \
        --enable-libx264 \
        --enable-libx265 \
        --enable-libsvtav1 \
        --enable-libopus \
        --enable-libvpx \
        --enable-libzimg \
        --enable-shared \
        --extra-cflags="-I/usr/local/include" \
        --extra-ldflags="-L/usr/local/lib" \
    && make -j"$(nproc)" \
    && make install \
    && cd /build \
    && rm -rf FFmpeg

FROM ubuntu:${UBUNTU_VERSION}

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        libdrm2 \
        libva2 \
        libx265-199 \
        mesa-va-drivers \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ffmpeg-builder /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg-builder /usr/local/bin/ffprobe /usr/local/bin/ffprobe
COPY --from=ffmpeg-builder /usr/local/lib/ /usr/local/lib/

RUN rm -rf /usr/local/lib/pkgconfig \
    && find /usr/local/lib -name '*.a' -delete \
    && ldconfig
