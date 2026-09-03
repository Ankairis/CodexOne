FROM node:24-alpine AS frontend
WORKDIR /src
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci
COPY web ./web
COPY internal/web ./internal/web
RUN cd web && npm run build

FROM golang:1.26.6-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=frontend /src/internal/web/dist ./internal/web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/codexone ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S codexone \
    && adduser -S -G codexone -u 10001 codexone \
    && mkdir -p /data \
    && chown -R codexone:codexone /data
COPY --from=builder /out/codexone /usr/local/bin/codexone
USER codexone
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/codexone"]
