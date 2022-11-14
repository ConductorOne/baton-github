FROM gcr.io/distroless/static-debian11:nonroot
ENTRYPOINT ["/baton-github"]
COPY baton-github /