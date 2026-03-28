# build
FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
# leverage module cache
COPY src/go.mod src/go.sum ./
RUN go mod download

COPY src/ .
RUN go build -trimpath -o /out/app .

# runtime
FROM alpine:latest
WORKDIR /app
RUN apk add --no-cache ffmpeg
COPY --from=build /out/app /app/app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["./app"]
CMD []
