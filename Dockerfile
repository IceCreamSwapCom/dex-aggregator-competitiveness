FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ ./cmd/...

# One image with every command: collector (the default), web, resim and distribution.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -H app
COPY --from=build /out/ /usr/local/bin/
COPY config /config
USER app
CMD ["collector", "-config", "/config/benchmark.json"]
