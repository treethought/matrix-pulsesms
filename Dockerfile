FROM golang:1.16.4-alpine3.13 AS builder

RUN apk add --no-cache git ca-certificates build-base su-exec olm-dev

WORKDIR /build
COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .
RUN ./build.sh
RUN mv ./pulsesms /usr/bin/matrix-pulsesms

FROM alpine:3.13
WORKDIR /app

ENV UID=1337 \
    GID=1337

RUN apk add --no-cache ffmpeg su-exec ca-certificates olm bash jq yq curl

COPY --from=builder /usr/bin/matrix-pulsesms /usr/bin/matrix-pulsesms
COPY --from=builder /build/example-config.yaml /opt/matrix-pulsesms/example-config.yaml
COPY --from=builder /build/docker-run.sh /docker-run.sh
VOLUME /data

# ENTRYPOINT ["/usr/bin/matrix-pulsesms"]

CMD ["/docker-run.sh"]
