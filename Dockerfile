# BUILDPLATFORM/TARGETOS/TARGETARCH are set by buildx. The defaults keep this
# buildable by the classic docker builder too, where they are undefined.
#
# Go version is pinned to a patch release: the floating 1.25-alpine tag can lag a
# stdlib security fix, and 1.25.11 shipped GO-2026-5856 in crypto/tls, which is in
# the path here because the exporter talks TLS to the Atera API.
FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.25.12-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-w -s -X main.version=$VERSION" \
    -o /out/atera-exporter ./cmd/atera-exporter

FROM gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.source="https://github.com/guyvolvo/atera-exporter"
LABEL org.opencontainers.image.description="Prometheus exporter for the Atera RMM API"

COPY --from=build /out/atera-exporter /atera-exporter
EXPOSE 9199
USER nonroot:nonroot
ENTRYPOINT ["/atera-exporter"]
