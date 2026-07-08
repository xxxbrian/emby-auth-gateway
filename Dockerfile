FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/gateway /usr/local/bin/gateway

EXPOSE 8090
ENTRYPOINT ["gateway"]
CMD ["serve", "--http=0.0.0.0:8090"]
