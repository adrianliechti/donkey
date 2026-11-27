FROM golang:1-bookworm AS idp
COPY hack/demo/fake-idp/main.go /src/main.go
RUN CGO_ENABLED=0 go build -o /demo-idp /src/main.go

FROM python:3-alpine
RUN pip install --no-cache-dir PyYAML==6.0.3
COPY --from=idp /demo-idp /demo-idp
COPY examples/service-policy/upstream.py /upstream.py
COPY examples/service-policy/observations.py /observations.py
USER 65534
CMD ["python3", "/upstream.py"]
