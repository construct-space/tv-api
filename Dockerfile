# Construct TV — Go service with embedded frontend (web/).
FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o construct-tv .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /app/construct-tv .
# CapRover routes to container port 80 by default — listen there in the container.
ENV PORT=80
EXPOSE 80
CMD ["./construct-tv"]
