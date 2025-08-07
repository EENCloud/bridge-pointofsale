FROM harbor.eencloud.com/dockerhub_proxy/library/golang:1.23-alpine AS build
WORKDIR /usr/src/app/bridge-devices-pos
ENV CGO_ENABLED=0
RUN apk add --no-cache bash git make
COPY . .
ENV GOTOOLCHAIN=local
COPY --from=harbor.eencloud.com/vms/goeen:1.0.314 /usr/src/app/go/src/github.com/eencloud/goeen /usr/src/app/goeen
RUN grep -v "replace github.com/eencloud/goeen" go.mod > go.mod.tmp && mv go.mod.tmp go.mod
RUN cat path_replacements.txt >> go.mod && go mod tidy
RUN go build -buildvcs=false -ldflags="-s -w" -o bridge-devices-pos ./cmd/bridge-devices-pos/

FROM harbor.eencloud.com/dockerhub_proxy/library/alpine:3.20 AS production
RUN apk add --no-cache tini supervisor
RUN mkdir -p /opt/een/app/point_of_sale /opt/een/data/point_of_sale /opt/een/var/log/point_of_sale
WORKDIR /opt/een/app/point_of_sale
COPY --from=build /usr/src/app/bridge-devices-pos/bridge-devices-pos .
COPY --from=build /usr/src/app/bridge-devices-pos/internal/bridge ./internal/bridge
COPY supervisord.conf /etc/supervisord.conf
COPY scripts/entrypoint.sh ./entrypoint.sh
RUN chmod +x ./entrypoint.sh
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["./entrypoint.sh"]

FROM production AS sim
COPY --from=build /usr/src/app/bridge-devices-pos/data ./data