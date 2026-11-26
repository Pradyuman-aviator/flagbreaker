# syntax=docker/dockerfile:1
# Builds the controller by default. Build the demo app with
#   docker build --build-arg CMD=./demo/checkout -t checkout-demo .
# Multi-arch images build natively on the builder's CPU and cross-compile:
#   docker buildx build --platform linux/amd64,linux/arm64 .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG CMD=./cmd/flagbreaker
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/app ${CMD}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
