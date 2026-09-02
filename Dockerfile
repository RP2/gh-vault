FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /gh-vault .

FROM alpine:3.21.4@sha256:b6a6be0ff92ab6db8acd94f5d1b7a6c2f0f5d10ce3c24af348d333ac6da80685
RUN apk add --no-cache git ca-certificates curl
COPY --from=build /gh-vault /usr/local/bin/gh-vault
# Run as non-root user.
RUN addgroup -g 568 apps && adduser -u 568 -G apps -D -s /bin/sh apps
USER apps
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
	CMD curl -kf https://localhost:8090/healthz || exit 1
ENTRYPOINT ["gh-vault"]
