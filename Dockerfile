FROM golang:1.26.8-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w" \
    -o /out/small-apple-mail ./cmd/small-apple-mail

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 -S sam \
    && adduser -u 10001 -S -D -H -G sam sam \
    && mkdir -p /app/data \
    && chown -R 10001:10001 /app

WORKDIR /app
COPY --from=builder --chown=10001:10001 /out/small-apple-mail /app/small-apple-mail

USER 10001:10001
EXPOSE 8787
ENTRYPOINT ["/app/small-apple-mail"]
CMD ["serve"]
