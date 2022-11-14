FROM gcr.io/distroless/static-debian11:nonroot
ENTRYPOINT ["/c1-github"]
COPY c1-github /