FROM golang:1.26-alpine AS build

ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/xxxbrian/emby-auth-gateway/internal/version.Version=${VERSION} -X github.com/xxxbrian/emby-auth-gateway/internal/version.Commit=${REVISION} -X github.com/xxxbrian/emby-auth-gateway/internal/version.Date=${BUILD_DATE}" -o /out/gateway ./cmd/gateway

FROM --platform=$BUILDPLATFORM alpine:3.22 AS runtime-assets
RUN apk add --no-cache ca-certificates tzdata

FROM alpine:3.22 AS runtime-base

ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.revision="${REVISION}"

COPY --from=runtime-assets /etc/ssl/certs/ /etc/ssl/certs/
COPY --from=runtime-assets /usr/share/zoneinfo/ /usr/share/zoneinfo/
WORKDIR /app

EXPOSE 8090
ENTRYPOINT ["gateway"]
CMD ["serve", "--http=0.0.0.0:8090"]

FROM runtime-base AS release
COPY build/docker/gateway /usr/local/bin/gateway

FROM runtime-base AS source
COPY --from=build /out/gateway /usr/local/bin/gateway
