FROM golang:1.26-alpine AS build

ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/xxxbrian/emby-auth-gateway/internal/version.Version=${VERSION} -X github.com/xxxbrian/emby-auth-gateway/internal/version.Commit=${REVISION} -X github.com/xxxbrian/emby-auth-gateway/internal/version.Date=${BUILD_DATE}" -o /out/gateway ./cmd/gateway

FROM alpine:3.22

ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.revision="${REVISION}"

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/gateway /usr/local/bin/gateway

EXPOSE 8090
ENTRYPOINT ["gateway"]
CMD ["serve", "--http=0.0.0.0:8090"]
