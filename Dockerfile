# syntax=docker/dockerfile:1

# --- build (pure-Go, no cgo => static binary; cross-compiled to the target arch) ---
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/dnttg ./cmd/dnttg && mkdir -p /out/data

# --- runtime (distroless, non-root; templates/static/migrations are embedded) ---
# /data is pre-created owned by the nonroot user (65532) so a fresh named volume
# inherits that ownership. Upgrading a root-owned volume from an older image:
#   docker run --rm -v dnttg-data:/data alpine chown -R 65532:65532 /data
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/dnttg /app/dnttg
COPY --from=build --chown=65532:65532 /out/data /data
ENV DNTTG_ADDR=:8080 \
    DNTTG_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/app/dnttg"]
CMD ["serve"]
