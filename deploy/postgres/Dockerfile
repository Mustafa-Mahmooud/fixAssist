# Postgres 16 + pgvector + WAL-G for continuous WAL archiving to R2 (S3 API).
FROM pgvector/pgvector:pg16
ARG WALG_VERSION=v3.0.10
ARG TARGETARCH
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    case "$TARGETARCH" in \
      arm64) arch=aarch64 ;; \
      amd64) arch=amd64 ;; \
      *) echo "unsupported arch $TARGETARCH"; exit 1 ;; \
    esac; \
    name="wal-g-pg-22.04-$arch"; \
    base="https://github.com/wal-g/wal-g/releases/download/$WALG_VERSION"; \
    curl -fsSL -o /usr/local/bin/wal-g "$base/$name"; \
    curl -fsSL -o /tmp/wal-g.sha256 "$base/$name.sha256"; \
    echo "$(cut -d' ' -f1 /tmp/wal-g.sha256)  /usr/local/bin/wal-g" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/wal-g; \
    apt-get purge -y curl; apt-get autoremove -y; \
    rm -rf /var/lib/apt/lists/* /tmp/*; \
    wal-g --version
