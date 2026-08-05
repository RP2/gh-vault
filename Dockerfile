FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /gh-vault .

FROM alpine:3.21
RUN apk add --no-cache git ca-certificates
COPY --from=build /gh-vault /usr/local/bin/gh-vault
# TrueNAS SCALE apps user: UID 568, GID 568 (group "apps")
# Run as this user so bind-mounted datasets with owner 568:568 work.
RUN addgroup -g 568 apps && adduser -u 568 -G apps -D -s /bin/sh apps
USER apps
ENTRYPOINT ["gh-vault"]
