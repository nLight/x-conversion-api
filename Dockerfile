FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/conversion-api ./cmd/conversion-api \
    && mkdir -m 0700 /out/data && chown 65532:65532 /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/conversion-api /conversion-api
COPY --from=build --chown=65532:65532 /out/data /data
COPY config/apps.example.json /config/apps.json
VOLUME ["/data"]
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/conversion-api", "-listen", ":8080", "-data", "/data", "-config", "/config/apps.json"]
