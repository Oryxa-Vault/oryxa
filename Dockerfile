# Static binary, no cgo, scratch image. The viewer is embedded via go:embed, so
# there is nothing to serve alongside it and nothing to mount.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Only the server. oryxa-shim is deliberately not here: it exists to start
# processes, and the things it starts are a CLI, its credentials and a
# repository — none of which are in a scratch image, and none of which should be
# handed to a container to keep the image small. Run it on the host.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /oryxa ./cmd/oryxa

FROM scratch
# Needed to reach agents and databases over TLS.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /oryxa /oryxa

# Connectors are configuration; mount your own over this.
COPY connectors/templates /connectors/templates

EXPOSE 8080
ENTRYPOINT ["/oryxa"]
CMD ["serve", "-addr", ":8080", "-connectors", "/connectors"]
