# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOFLAGS=-trimpath go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/ward ./cmd/ward

FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
RUN apt-get update \
    && apt-get install -y --no-install-recommends podman catatonit ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ward /usr/local/bin/ward
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/ward", "runner"]
