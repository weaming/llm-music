FROM scratch
COPY certs/ca-certificates.crt /etc/ssl/certs/
COPY bin-tmp/llm-server /app/llm-server
ENV LLM_SERVER_PORT=8080
EXPOSE 8080
CMD ["/app/llm-server"]
