# Multi-stage build: a static binary on a distroless, non-root image.
# The build stage runs on the build host and cross-compiles for the target.
FROM --platform=$BUILDPLATFORM golang:1.25.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/imap-mcp ./cmd/imap-mcp

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/imap-mcp /usr/local/bin/imap-mcp
ENV IMAP_MCP_LISTEN=0.0.0.0:8787
EXPOSE 8787
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/imap-mcp"]
