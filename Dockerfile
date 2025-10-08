FROM golang:1.25-alpine AS build
RUN apk add --no-cache git build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o /autopg ./main.go

FROM alpine:3.18
RUN apk add --no-cache ca-certificates
COPY --from=build /autopg /usr/local/bin/autopg
USER 1000
ENTRYPOINT ["/usr/local/bin/autopg"]