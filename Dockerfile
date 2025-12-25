FROM golang:1.22-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
ARG TARGETVARIANT
RUN set -eu; \
  export CGO_ENABLED=0; \
  export GOOS="${TARGETOS}"; \
  export GOARCH="${TARGETARCH}"; \
  if [ "${TARGETARCH}" = "arm" ]; then \
    v="${TARGETVARIANT#v}"; \
    if [ -z "${v}" ] || [ "${v}" = "${TARGETVARIANT}" ]; then v="7"; fi; \
    export GOARM="${v}"; \
  fi; \
  go build -trimpath -ldflags="-s -w" -o /out/rclone-syncd ./cmd/115togd

FROM rclone/rclone:latest AS runtime-official
COPY --from=build /out/rclone-syncd /usr/local/bin/rclone-syncd
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8
ENV RCLONE_CONFIG=/data/rclone.conf
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

FROM debian:bookworm-slim AS runtime-115
RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates curl bash tar gzip unzip \
  && rm -rf /var/lib/apt/lists/*
# Enable UTF-8 locale for better Chinese filename display in shell/tools.
ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8
# Install wiserain rclone (includes 115 support). In container build we are already root, no sudo needed.
RUN curl -fsSL https://raw.githubusercontent.com/wiserain/rclone/mod/install.sh | bash
COPY --from=build /out/rclone-syncd /usr/local/bin/rclone-syncd
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENV RCLONE_CONFIG=/data/rclone.conf
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
