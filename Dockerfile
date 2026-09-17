FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags '-s -w' -o /openmind ./cmd/openmind

FROM alpine:3.20
RUN adduser -D -h /data openmind
COPY --from=build /openmind /usr/local/bin/openmind
USER openmind
VOLUME /data
ENV OPENMIND_DB=/data/openmind.db OPENMIND_ADDR=:7777
EXPOSE 7777
ENTRYPOINT ["openmind"]
CMD ["serve"]
