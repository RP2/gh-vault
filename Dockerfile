FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /gh-vault .

FROM alpine:3.21.4@sha256:46fac42d1232a09876243d83e0f67c3b4d5e0b73b0b782a50e7c53e3f03b473a
RUN apk add --no-cache git ca-certificates
COPY --from=build /gh-vault /usr/local/bin/gh-vault
# Run as non-root user.
RUN addgroup -g 568 apps && adduser -u 568 -G apps -D -s /bin/sh apps
USER apps
ENTRYPOINT ["gh-vault"]
