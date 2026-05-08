FROM golang:1.22-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN go mod tidy && go build -ldflags="-s -w" -o /out/collector .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/collector /collector
EXPOSE 8080
ENTRYPOINT ["/collector"]
