FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /backup-service .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /backup-service /usr/local/bin/backup-service
EXPOSE 8080
ENTRYPOINT ["backup-service"]
