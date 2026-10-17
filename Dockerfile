# --- Build ---
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mirc-chatserver .

# --- Laufzeit ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S mirc && adduser -S -G mirc -H mirc \
 && mkdir -p /data && chown mirc:mirc /data
COPY --from=build /out/mirc-chatserver /usr/local/bin/mirc-chatserver
ENV MIRC_LISTEN=:6680 \
    MIRC_TLS_LISTEN=:6697 \
    MIRC_DATA=/data \
    MIRC_SESSION_URL=http://localhost:8440 \
    TZ=Europe/Berlin
USER mirc
VOLUME ["/data"]
EXPOSE 6680 6697
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD nc -z 127.0.0.1 6697 || exit 1
ENTRYPOINT ["/usr/local/bin/mirc-chatserver"]
