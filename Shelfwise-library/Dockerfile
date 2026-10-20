# One Dockerfile builds any service: docker build --build-arg SERVICE=services/catalog .
FROM golang:1.24-alpine AS build
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app ./${SERVICE}

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget
COPY --from=build /app /app
ENTRYPOINT ["/app"]
