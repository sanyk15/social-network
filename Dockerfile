FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server
RUN CGO_ENABLED=0 go build -o /out/seed ./cmd/seed
RUN CGO_ENABLED=0 go build -o /out/seedfeed ./cmd/seedfeed
RUN CGO_ENABLED=0 go build -o /out/feedrebuild ./cmd/feedrebuild

FROM alpine:3.20 AS app
RUN apk add --no-cache ca-certificates
COPY --from=build /out/server /usr/local/bin/server
EXPOSE 8080
ENTRYPOINT ["server"]

FROM alpine:3.20 AS seed
RUN apk add --no-cache ca-certificates
COPY --from=build /out/seed /usr/local/bin/seed
ENTRYPOINT ["seed"]

FROM alpine:3.20 AS seedfeed
RUN apk add --no-cache ca-certificates
COPY --from=build /out/seedfeed /usr/local/bin/seedfeed
ENTRYPOINT ["seedfeed"]

FROM alpine:3.20 AS feedrebuild
RUN apk add --no-cache ca-certificates
COPY --from=build /out/feedrebuild /usr/local/bin/feedrebuild
ENTRYPOINT ["feedrebuild"]
