ARG UBUNTU_VERSION=24.04

FROM ubuntu:${UBUNTU_VERSION}

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        autoconf \
        automake \
        build-essential \
        ca-certificates \
        cmake \
        git \
        libtool \
        nasm \
        pkg-config \
        wget \
        yasm \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

RUN git clone --depth 1 --branch stable https://github.com/mirror/x264.git \
    && cd x264 \
    && ./configure --prefix=/usr/local --enable-shared --enable-pic \
    && make -j"$(nproc)" \
    && make install \
    && cd /build \
    && rm -rf x264

RUN git clone --depth 1 --branch v2.3.0 https://gitlab.com/AOMediaCodec/SVT-AV1.git \
    && cd SVT-AV1 \
    && mkdir build \
    && cd build \
    && cmake -G "Unix Makefiles" \
        -DCMAKE_INSTALL_PREFIX=/usr/local \
        -DBUILD_SHARED_LIBS=ON \
        -DBUILD_TESTING=OFF \
        -DBUILD_APPS=OFF \
        .. \
    && make -j"$(nproc)" \
    && make install \
    && cd /build \
    && rm -rf SVT-AV1

RUN git clone --depth 1 --branch v1.5.2 https://github.com/xiph/opus.git \
    && cd opus \
    && autoreconf -fiv \
    && ./configure --prefix=/usr/local --enable-shared \
    && make -j"$(nproc)" \
    && make install \
    && cd /build \
    && rm -rf opus

RUN git clone --depth 1 --branch v1.15.2 https://github.com/webmproject/libvpx.git \
    && cd libvpx \
    && ./configure --prefix=/usr/local \
        --enable-shared \
        --enable-vp9 \
        --enable-vp9-highbitdepth \
        --enable-pic \
        --disable-examples \
        --disable-docs \
        --disable-unit-tests \
    && make -j"$(nproc)" \
    && make install \
    && cd /build \
    && rm -rf libvpx

RUN wget -qO zimg.tar.gz https://github.com/sekrit-twc/zimg/archive/refs/tags/release-3.0.6.tar.gz \
    && tar -xzf zimg.tar.gz \
    && cd zimg-release-3.0.6 \
    && autoreconf -fiv \
    && ./configure --prefix=/usr/local --enable-shared \
    && make -j"$(nproc)" \
    && make install \
    && cd /build \
    && rm -rf zimg-release-3.0.6 zimg.tar.gz
