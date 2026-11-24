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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
EXPOSE 8787
ENTRYPOINT ["/app"]
CMD ["serve", "--addr=:8787"]
