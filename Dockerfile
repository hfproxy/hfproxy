ARG GO_VERSION=1.27.3
ARG NODE_VERSION=24.22.1-alpine

FROM node:${NODE_VERSION} AS ui
WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

FROM golang:${GO_VERSION}-alpine AS build
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown

RUN apk add --no-cache ca-certificates git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /ui/dist/hfproxy/browser ./internal/server/web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${BUILD_VERSION} -X github.com/nicolas/hfproxy/internal/server.Version=${BUILD_VERSION} -X main.commit=${BUILD_COMMIT}" -o /out/hfproxy ./cmd/hfproxy

FROM alpine:3.24
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 65532 hfproxy \
    && adduser -S -D -H -u 65532 -G hfproxy hfproxy
COPY --from=build /out/hfproxy /usr/local/bin/hfproxy
USER 65532:65532
ENV HFPROXY_LISTEN_ADDR=0.0.0.0:8080
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 CMD ["/usr/local/bin/hfproxy", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/hfproxy"]
CMD ["serve"]
