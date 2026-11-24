FROM golang:1.24 AS build
WORKDIR /src
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /mock-dce .
FROM busybox:1.36
COPY --from=build /mock-dce /mock-dce
EXPOSE 8090
ENTRYPOINT ["/mock-dce"]
