package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("LLM_SERVER_PORT")
	if port == "" {
		port = "8080"
	}

	addr := ":" + port
	log.Printf("LLM Server 启动于 %s", addr)
	log.Printf("LLM 配置: base_url=%s model=%s", currentOpenAIBaseURL(), currentOpenAIModel())

	srv := &http.Server{
		Addr:         addr,
		Handler:      router(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: upstreamTimeout,
		IdleTimeout:  60 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("服务器启动失败: %v", err)
	}
}
