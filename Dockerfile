# Build Go binary
FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /graph ./cmd/graph

# Runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /graph /graph
RUN mkdir -p /data
ENV PORT=8080
EXPOSE 8080
CMD ["/graph"]
