# syntax=docker/dockerfile:1
# The single Fix Assistant binary. Cross-compiles natively on the build
# machine (no emulation), so building linux/arm64 on an amd64 runner is fast.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/app

# Visitor countries: this month's DB-IP Lite database (CC BY 4.0, attributed
# on the dashboard), baked into the image so hosts without a disk or cron
# (Northflank, Render, ...) still have it. A rebuild refreshes it. If the
# download fails the app still runs, without countries.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS geo
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /geo && cd /geo && \
    for m in "$(date -u +%Y-%m)" "$(date -u -d "$(date -u +%Y-%m-01) -1 day" +%Y-%m)"; do \
      curl -fsSL "https://download.db-ip.com/free/dbip-country-lite-$m.mmdb.gz" -o db.gz && gunzip -f db.gz && mv db dbip.mmdb && break; \
    done; \
    [ -s dbip.mmdb ] || : > dbip.mmdb

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
COPY --from=geo /geo/dbip.mmdb /geo/dbip.mmdb
ENV GEOIP_DB=/geo/dbip.mmdb
USER nonroot:nonroot
EXPOSE 8787
ENTRYPOINT ["/app"]
# Platforms that assign a port set PORT; the app also reads it.
CMD ["serve", "--addr=:8787"]
