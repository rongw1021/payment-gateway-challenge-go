FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY main.go ./
COPY internal ./internal
COPY docs ./docs
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/payment-gateway .

FROM alpine:3.23

RUN addgroup -S gateway && adduser -S -G gateway gateway
WORKDIR /app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/payment-gateway /usr/local/bin/payment-gateway

USER gateway
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/payment-gateway"]
