# Multi-stage build: static binary (pure-Go SQLite, no cgo) on a small Alpine base.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mcp-imdb ./src/cmd/mcp-imdb

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata tini && adduser -D -H -u 10001 app \
    && mkdir /data && chown app:app /data
COPY --from=build /out/mcp-imdb /usr/local/bin/mcp-imdb
USER app
ENV LISTEN=:8810 DATA_DIR=/data
VOLUME /data
EXPOSE 8810
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8810/api/healthz >/dev/null || exit 1
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/mcp-imdb"]
