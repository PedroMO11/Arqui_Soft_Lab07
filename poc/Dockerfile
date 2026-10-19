# Imagen unica con los 3 binarios (blackbox, api, loadgen).
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/...

FROM alpine:3.20
COPY --from=build /out/ /usr/local/bin/
