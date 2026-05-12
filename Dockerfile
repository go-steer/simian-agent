# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/simian ./cmd/simian

FROM gcr.io/distroless/static
COPY --from=build /out/simian /usr/local/bin/simian
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/simian"]
CMD ["serve"]
