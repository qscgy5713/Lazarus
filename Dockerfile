# Builds a static Lazarus binary, then packages it with the tools it shells
# out to at runtime: the docker CLI (to run sandbox containers — talking to
# the *host's* Docker daemon via the mounted socket, not a nested one),
# sqlite3 (for sqlite targets, which never touch Docker at all), and gpg
# (for GPG-encrypted backups). None of these are optional add-ons: leaving
# one out just means whichever target needs it fails with "executable file
# not found" instead of a clean feature gap.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /lazarus ./cmd/lazarus

FROM alpine:3.20
RUN apk add --no-cache docker-cli sqlite gnupg ca-certificates
COPY --from=build /lazarus /usr/local/bin/lazarus
ENTRYPOINT ["lazarus"]
CMD ["--config", "/etc/lazarus/lazarus.yml"]
