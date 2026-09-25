# Base images pinned by digest (not just tag). On the game-host self-hosted runner,
# dockerd/BuildKit resolve a *tag* by HEADing registry-1.docker.io, and that
# request goes out over IPv6 — which has no route on the host — so every deploy
# flaked on `load metadata for golang:1.23-alpine` even though the image was
# already cached locally. A digest ref is satisfied from the local content store
# with no registry round-trip, so cached builds stop hitting docker.io. The host
# sysctl `disable_ipv6` is already persisted but ignored by BuildKit's own Go
# resolver, so this CI-side pin is the durable fix. To bump: pull the new tag
# once on the runner, read the resulting digest, and update the line below.
FROM golang:1.23-alpine@sha256:383395b794dffa5b53012a212365d40c8e37109a626ca30d6151c8348d380b5f AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/ops-api ./

# Runtime image. We need `git` for the git_pull_* MCP tools — distroless
# static doesn't include it, and a multi-stage copy of git pulls in too
# many shared libs. debian-slim adds ~70 MB but keeps git+ca-certs+sh
# available. nonroot uid 65532 is preserved so existing /feedback and
# /backups/wow-conf chowns continue to match the in-container uid.
FROM debian:bookworm-slim@sha256:96e378d7e6531ac9a15ad505478fcc2e69f371b10f5cdf87857c4b8188404716
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system --gid 65532 nonroot \
 && useradd --system --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin nonroot
USER nonroot:nonroot
COPY --from=build /out/ops-api /ops-api
EXPOSE 8080
ENTRYPOINT ["/ops-api"]
