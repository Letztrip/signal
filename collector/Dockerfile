FROM golang:1.25-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN go mod tidy && go build -ldflags="-s -w" -o /out/collector .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/collector /collector
# GeoLite2-City.mmdb is committed under collector/geoip/ (see geoip/README.md).
# If the file is absent the collector still boots — geo fields stay empty.
COPY geoip/ /geoip/
ENV GEOIP_DB_PATH=/geoip/GeoLite2-City.mmdb
EXPOSE 8080
ENTRYPOINT ["/collector"]
