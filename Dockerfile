# syntax=docker/dockerfile:1
# Static, zero-CGO build (modernc.org/sqlite is pure Go), shipped in a tiny
# distroless image. The binary runs against a working directory that holds
# config.yaml, secrets.yaml, skills/ and state.db (see docker-compose.yml).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/draftcat .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/draftcat /usr/local/bin/draftcat
# Bundle default config + skills so the image boots standalone (one-click PaaS
# deploys). Tokens/operator IDs come from env; persist state.db on a mounted
# volume via DRAFTCAT_STATE_PATH so the volume doesn't shadow these files.
COPY --from=build /src/config.yaml /work/config.yaml
COPY --from=build /src/skills /work/skills
WORKDIR /work
EXPOSE 8088
ENTRYPOINT ["draftcat"]
