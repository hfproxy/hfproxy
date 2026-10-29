# Base images are pinned by digest so a build is reproducible and a tag cannot
# be moved under us; Dependabot proposes new digests.
FROM node:26.12.1-alpine@sha256:40cd011b55415bc894e45b02fdd5a5dfde056921b7d23a64c74f80497af48797 AS ui
WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown

RUN apk add --no-cache ca-certificates git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /ui/dist/hfproxy/browser ./internal/server/web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${BUILD_VERSION} -X github.com/hfproxy/hfproxy/internal/server.Version=${BUILD_VERSION} -X main.commit=${BUILD_COMMIT}" -o /out/hfproxy ./cmd/hfproxy

# Only the binary, for `docker buildx build --target binary --output type=local`:
# the release workflow ships exactly the file the image contains. Keep this stage
# before the final one, which must stay last so a plain build still gives the image.
FROM scratch AS binary
COPY --from=build /out/hfproxy /hfproxy

# The final image holds the static binary and CA certificates only: no shell,
# no package manager. It runs as UID 65532 (nonroot).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/hfproxy /usr/local/bin/hfproxy
USER 65532:65532
ENV HFPROXY_LISTEN_ADDR=0.0.0.0:8080
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 CMD ["/usr/local/bin/hfproxy", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/hfproxy"]
CMD ["serve"]
