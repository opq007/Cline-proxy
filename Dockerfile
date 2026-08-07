FROM golang:1.26-alpine AS builder

WORKDIR /build
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o cline-proxy .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /build/cline-proxy .

# 持久化目录（由 compose 挂载 ./data）
RUN mkdir -p /app/data
VOLUME ["/app/data"]

EXPOSE 3457

ENV PORT=3457
ENV DATA_DIR=/app/data
# 公网部署请设置 ADMIN_TOKEN（或 CLINE_ADMIN_TOKEN / -admin-token）
# ENV ADMIN_TOKEN=

ENTRYPOINT ["/app/cline-proxy"]
CMD ["-host", "0.0.0.0", "-port", "3457"]
