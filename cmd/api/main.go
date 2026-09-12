// Command api serves the trackside pulse detection HTTP API.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"trackside-pulse-api/internal/api"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false,
		"probe the local API once and exit with the probe status")
	flag.Parse()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if *healthcheck {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			log.Printf("healthcheck failed: %v", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("healthcheck status %d", resp.StatusCode)
			os.Exit(1)
		}
		return
	}

	router := api.NewRouter()
	addr := ":" + port
	log.Printf("trackside pulse API listening on %s", addr)
	if err := router.Run(addr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
