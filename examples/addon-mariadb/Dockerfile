# Build from the repository root:
# docker build -f examples/addon-mariadb/Dockerfile -t donkey-addon-mariadb:dev .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg/addons ./pkg/addons
COPY examples/addon-mariadb ./examples/addon-mariadb
RUN CGO_ENABLED=0 go build -o /mariadb-plugin ./examples/addon-mariadb

FROM gcr.io/distroless/static:nonroot
COPY --from=build /mariadb-plugin /mariadb-plugin
ENTRYPOINT ["/mariadb-plugin"]
