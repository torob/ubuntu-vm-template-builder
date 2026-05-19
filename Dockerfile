# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.2-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/ubuntu-vm-template-builder .

FROM docker.io/library/ubuntu:26.04 AS runtime

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        ovmf \
        qemu-system-x86 \
        qemu-utils \
        ubuntu-keyring \
        xorriso \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/ubuntu-vm-template-builder /usr/local/bin/ubuntu-vm-template-builder

WORKDIR /work

ENTRYPOINT ["ubuntu-vm-template-builder"]
CMD ["--help"]
