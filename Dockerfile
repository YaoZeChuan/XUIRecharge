FROM golang:1.27-alpine AS build
RUN apk add --no-cache ca-certificates git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/recharge ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 app \
    && adduser -S -D -H -u 10001 -G app app \
    && mkdir -p /app/data /app/configs \
    && chown -R app:app /app
WORKDIR /app
COPY --from=build --chown=app:app /out/recharge /app/recharge
COPY --chown=app:app configs/config.example.yaml /app/configs/config.example.yaml
USER 10001:10001
ENV SERVER_HOST=0.0.0.0 SERVER_PORT=8080 DATABASE_PATH=/app/data/recharge.db CONFIG_FILE=/app/configs/config.example.yaml
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/api/healthz || exit 1
ENTRYPOINT ["/app/recharge"]
