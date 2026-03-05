FROM golang:1.22 AS builder

WORKDIR /workspace

COPY . bytetrade.io/web3os/l4-bfl-proxy/

RUN cd bytetrade.io/web3os/l4-bfl-proxy/ && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o l4-bfl-proxy ./cmd/

FROM envoyproxy/envoy:v1.32-latest
WORKDIR /

COPY --from=builder /workspace/bytetrade.io/web3os/l4-bfl-proxy/l4-bfl-proxy .

ENTRYPOINT ["/l4-bfl-proxy"]
