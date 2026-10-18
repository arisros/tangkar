FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tangkar ./cmd/tangkar

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tangkar /tangkar
ENV TANGKAR_ADDR=:8080 TANGKAR_DB=/data/tangkar.db
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/tangkar", "serve"]
