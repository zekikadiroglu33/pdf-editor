# --- build stage -------------------------------------------------------
FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY static ./static
COPY assets ./assets
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /pdfer .

# --- runtime stage -------------------------------------------------------
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends qpdf ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

RUN useradd --system --uid 10001 --home-dir /nonexistent --shell /usr/sbin/nologin pdfer
COPY --from=build /pdfer /usr/local/bin/pdfer
USER pdfer

ENV PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD curl -fsS http://127.0.0.1:8080/api/health || exit 1

ENTRYPOINT ["/usr/local/bin/pdfer"]
