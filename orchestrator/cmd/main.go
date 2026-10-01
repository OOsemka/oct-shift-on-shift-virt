package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/api"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/logging"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/orchestrator"
)

func main() {
	logger := logging.New()
	logger.Info("starting oct-shift-on-shift-virt orchestrator")

	kubeconfig := os.Getenv("KUBECONFIG")
	client, err := k8s.NewClient(kubeconfig)
	if err != nil {
		logger.Error("failed to create kubernetes client", "error", err.Error())
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	engine := orchestrator.NewEngine(client, logger)
	go func() {
		if err := engine.Watch(ctx); err != nil && ctx.Err() == nil {
			logger.Error("engine watch failed", "error", err.Error())
		}
	}()

	apiServer := api.NewServer(client, logger)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8443"
	}

	certFile := os.Getenv("TLS_CERT")
	if certFile == "" {
		certFile = "/var/serving-cert/tls.crt"
	}
	keyFile := os.Getenv("TLS_KEY")
	if keyFile == "" {
		keyFile = "/var/serving-cert/tls.key"
	}

	server := &http.Server{
		Addr:         fmt.Sprintf(":%s", port),
		Handler:      apiServer.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	if _, err := os.Stat(certFile); err == nil {
		logger.Info("starting HTTPS server", "port", port)
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			logger.Error("failed to load TLS cert", "error", err.Error())
			os.Exit(1)
		}
		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		go func() {
			if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				logger.Error("HTTPS server failed", "error", err.Error())
				cancel()
			}
		}()
	} else {
		logger.Info("TLS cert not found, starting HTTP server", "port", port)
		go func() {
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("HTTP server failed", "error", err.Error())
				cancel()
			}
		}()
	}

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	server.Shutdown(shutdownCtx)
}
