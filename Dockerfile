FROM golang:1.26.10-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /relay-sync .

FROM alpine:3.22.7
RUN apk add --no-cache ca-certificates
COPY --from=build /relay-sync /usr/local/bin/relay-sync
ENTRYPOINT ["relay-sync"]
