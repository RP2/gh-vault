FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /gh-vault .

FROM alpine:3.21
RUN apk add --no-cache git ca-certificates
COPY --from=build /gh-vault /usr/local/bin/gh-vault
RUN addgroup -S ghvault && adduser -S ghvault -G ghvault
USER ghvault
ENTRYPOINT ["gh-vault"]
