# TARGETOS/TARGETARCH are supplied by buildx. Hardcoding amd64 here would make
# the multi-arch build in release.yml silently produce amd64 binaries inside
# arm64 images.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-w -s -X main.version=$VERSION" \
    -o /out/atera-exporter ./cmd/atera-exporter

FROM gcr.io/distroless/static:nonroot

# Links the GHCR package to the repo, so the package page shows the README and
# inherits provenance instead of being an orphan.
LABEL org.opencontainers.image.source="https://github.com/guyvolvo/atera-exporter"
LABEL org.opencontainers.image.description="Prometheus exporter for the Atera RMM API"

COPY --from=build /out/atera-exporter /atera-exporter
EXPOSE 9199
USER nonroot:nonroot
ENTRYPOINT ["/atera-exporter"]
