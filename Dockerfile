FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
RUN go build -o /tmp/wherenow ./cmd

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /tmp/wherenow /usr/local/bin/wherenow
ENTRYPOINT ["wherenow", "-log-file", "/data/geo.log.jsonl"]
