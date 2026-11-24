FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /lab ./cmd/lab

FROM alpine:3.21
COPY --from=build /lab /lab
EXPOSE 8080
EXPOSE 8081 8091 5353/udp
CMD ["/lab"]
