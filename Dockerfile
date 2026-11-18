# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27
ARG RCLONE_VERSION=1.76.0

FROM docker.io/library/golang:${GO_VERSION}-trixie AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go test ./... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bentopdf-s3 .

# The official rclone image (Alpine) already provides rclone, CA
# certificates and /etc/mime.types, which rclone uses to set Content-Type.
FROM docker.io/rclone/rclone:${RCLONE_VERSION}
COPY --from=build /out/bentopdf-s3 /usr/local/bin/bentopdf-s3
ENV WORK_DIR=/work HOME=/work
RUN mkdir -p /work && chown 65532:65532 /work
USER 65532:65532
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/bentopdf-s3"]
