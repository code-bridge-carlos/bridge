FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /s1-panel .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /s1-panel /s1-panel
EXPOSE 8080
CMD ["/s1-panel", "-port", "8080"]
