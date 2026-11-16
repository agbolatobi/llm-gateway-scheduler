FROM golang:1.26.6@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6 AS build
WORKDIR /source
COPY gateway/ ./
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/gateway ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/container-util ./cmd/container-util
FROM scratch
COPY --from=build /out/ /
USER 10001:10001
ENV HOME=/tmp
EXPOSE 8000
ENTRYPOINT ["/gateway"]
