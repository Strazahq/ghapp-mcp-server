# Build a fully static binary, then ship it on scratch.
#
# The build stage runs on the build machine's own platform and cross-compiles
# for the target platform, so a multi-arch build needs no emulation.
# The Go release is the toolchain line of go.mod, pinned by digest. Dependabot
# moves the tag and the digest together.
FROM --platform=$BUILDPLATFORM golang:1.26.10@sha256:670bb5aa5349ae03b8516764718486f4f16399134bdc9acd0c46a23f043ab05c AS build

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off: a static, pure-Go binary that runs on scratch.
ARG TARGETOS TARGETARCH
ARG VERSION=docker
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" -o /out/ghapp-mcp-server .

# Runtime: scratch (nothing but the binary and CA certs).
FROM scratch

# CA certificates so the server can reach GitHub over HTTPS.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ghapp-mcp-server /ghapp-mcp-server

# Run unprivileged (scratch has no users; use the conventional "nobody" uid).
USER 65534:65534

# ghapp works only behind Straza: run it with --http and an address such as
# 0.0.0.0:3003, with GHAPP_MCP_GATEWAY_SECRET or the OIDC settings.
# Configuration is via GHAPP_* environment variables at runtime, the App's
# private key best as a mounted file named by GHAPP_PRIVATE_KEY_FILE. --dev,
# for development only, serves stdio or loopback without caller
# authentication.
ENTRYPOINT ["/ghapp-mcp-server"]
