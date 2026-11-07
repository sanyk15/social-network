FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server
RUN CGO_ENABLED=0 go build -o /out/seed ./cmd/seed

FROM alpine:3.20 AS app
RUN apk add --no-cache ca-certificates
COPY --from=build /out/server /usr/local/bin/server
EXPOSE 8080
ENTRYPOINT ["server"]

FROM alpine:3.20 AS seed
RUN apk add --no-cache ca-certificates
COPY --from=build /out/seed /usr/local/bin/seed
ENTRYPOINT ["seed"]
