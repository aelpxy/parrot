# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

# git lets `go build` stamp the version from the checkout's tags.
RUN apk add --no-cache git

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    git config --global --add safe.directory /src && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/parrot ./cmd/parrot && \
    mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/parrot /usr/local/bin/parrot
COPY --from=build --chown=nonroot:nonroot /out/data /data

# parrot keeps tokens and certificates in $STATE_DIRECTORY.
ENV STATE_DIRECTORY=/data
VOLUME /data

EXPOSE 80 443 7000

ENTRYPOINT ["/usr/local/bin/parrot"]
CMD ["--help"]
