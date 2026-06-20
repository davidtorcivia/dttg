# --- build (pure-Go, no cgo => static binary) ---
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/dnttg ./cmd/dnttg

# --- runtime (distroless; templates/static/migrations are embedded in the binary) ---
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/dnttg /app/dnttg
ENV DNTTG_ADDR=:8080 \
    DNTTG_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/app/dnttg"]
CMD ["serve"]
