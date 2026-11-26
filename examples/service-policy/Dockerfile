FROM golang:1-bookworm AS idp
COPY hack/demo/fake-idp/main.go /src/main.go
RUN CGO_ENABLED=0 go build -o /demo-idp /src/main.go

FROM python:3-alpine
COPY --from=idp /demo-idp /demo-idp
COPY examples/service-policy/upstream.py /upstream.py
USER 65534
CMD ["python3", "/upstream.py"]
