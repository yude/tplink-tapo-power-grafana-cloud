# syntax=docker/dockerfile:1.7
FROM golang:1.26.8-alpine AS build

ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/tapo-grafana-collector \
    ./cmd/tapo-grafana-collector

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tapo-grafana-collector /tapo-grafana-collector
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/tapo-grafana-collector"]
