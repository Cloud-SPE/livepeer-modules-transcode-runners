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
        curl \
        git \
        gpg \
        libssl-dev \
        libx265-dev \
        nasm \
        pkg-config \
        yasm \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://repositories.intel.com/gpu/intel-graphics.key | gpg --dearmor -o /usr/share/keyrings/intel-graphics.gpg \
    && echo "deb [arch=amd64 signed-by=/usr/share/keyrings/intel-graphics.gpg] https://repositories.intel.com/gpu/ubuntu noble unified" > /etc/apt/sources.list.d/intel-gpu.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        intel-media-va-driver-non-free \
        libva-dev \
        libvpl-dev \
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
        --enable-libvpl \
        --enable-openssl \
        --enable-vaapi \
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
        libmfx-gen1.2 \
        libva2 \
        libvpl2 \
        libx265-199 \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN apt-get update \
    && apt-get install -y --no-install-recommends curl gpg \
    && curl -fsSL https://repositories.intel.com/gpu/intel-graphics.key | gpg --dearmor -o /usr/share/keyrings/intel-graphics.gpg \
    && echo "deb [arch=amd64 signed-by=/usr/share/keyrings/intel-graphics.gpg] https://repositories.intel.com/gpu/ubuntu noble unified" > /etc/apt/sources.list.d/intel-gpu.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends intel-media-va-driver-non-free \
    && apt-get purge -y --auto-remove curl gpg \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ffmpeg-builder /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg-builder /usr/local/bin/ffprobe /usr/local/bin/ffprobe
COPY --from=ffmpeg-builder /usr/local/lib/ /usr/local/lib/

RUN rm -rf /usr/local/lib/pkgconfig \
    && find /usr/local/lib -name '*.a' -delete \
    && ldconfig
