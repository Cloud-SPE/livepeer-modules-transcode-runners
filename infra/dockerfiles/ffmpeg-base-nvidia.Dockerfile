ARG REGISTRY=localbuild
ARG TAG=v1.3.0
ARG CUDA_VERSION=13.2.1
ARG UBUNTU_VERSION=24.04
ARG CODECS_IMAGE=${REGISTRY}/codecs-builder:${TAG}

FROM ${CODECS_IMAGE} AS codecs-builder

FROM nvidia/cuda:${CUDA_VERSION}-devel-ubuntu${UBUNTU_VERSION} AS ffmpeg-builder

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        git \
        libssl-dev \
        libx265-dev \
        nasm \
        pkg-config \
        yasm \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY --from=codecs-builder /usr/local /usr/local
RUN ldconfig

WORKDIR /build

RUN git clone --depth 1 --branch n12.2.72.0 https://github.com/FFmpeg/nv-codec-headers.git \
    && cd nv-codec-headers \
    && make install PREFIX=/usr/local \
    && cd /build \
    && rm -rf nv-codec-headers

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
        --enable-cuda-nvcc \
        --enable-openssl \
        --enable-nvenc \
        --enable-nvdec \
        --enable-cuvid \
        --enable-libx264 \
        --enable-libx265 \
        --enable-libsvtav1 \
        --enable-libopus \
        --enable-libvpx \
        --enable-libzimg \
        --enable-shared \
        --nvccflags="-gencode arch=compute_75,code=sm_75 -O2" \
        --extra-cflags="-I/usr/local/include -I/usr/local/cuda/include" \
        --extra-ldflags="-L/usr/local/lib -L/usr/local/cuda/lib64" \
    && make -j"$(nproc)" \
    && make install \
    && cd /build \
    && rm -rf FFmpeg

FROM nvidia/cuda:${CUDA_VERSION}-runtime-ubuntu${UBUNTU_VERSION}

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        libx265-199 \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ffmpeg-builder /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg-builder /usr/local/bin/ffprobe /usr/local/bin/ffprobe
COPY --from=ffmpeg-builder /usr/local/lib/ /usr/local/lib/

RUN rm -rf /usr/local/lib/pkgconfig \
    && find /usr/local/lib -name '*.a' -delete \
    && ldconfig
