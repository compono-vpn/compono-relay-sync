FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /relay-sync .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /relay-sync /usr/local/bin/relay-sync
ENTRYPOINT ["relay-sync"]
