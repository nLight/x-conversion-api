package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"conversion-api/internal/conversion"
	"conversion-api/internal/envfile"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config/apps.json", "application configuration file")
	dataPath := flag.String("data", "data", "durable outbox directory (one process per directory)")
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	envPath := flag.String("env-file", ".env", "dotenv file; empty disables loading")
	healthURL := flag.String("healthcheck", "", "check an HTTP health endpoint and exit without loading server configuration")
	flag.Parse()
	if *healthURL != "" {
		return checkHealth(*healthURL)
	}
	if err := envfile.Load(*envPath); err != nil {
		return err
	}
	config, err := conversion.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if err := config.ValidateIngestSecrets(); err != nil {
		return err
	}
	store, err := conversion.OpenStore(*dataPath)
	if err != nil {
		return err
	}
	defer store.Close()
	mobile, err := conversion.NewMobileAPI(config, store, os.Getenv("CONVERSION_TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *listen, Handler: &conversion.API{Config: config, Store: store, Mobile: mobile},
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	errorsCh := make(chan error, 2)
	workerDone := make(chan struct{})
	dispatcher := &conversion.Dispatcher{Config: config, Store: store, Client: conversion.NewXClient()}
	go func() {
		defer close(workerDone)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if err := dispatcher.DispatchOnce(ctx, now.UTC()); err != nil {
					errorsCh <- err
					return
				}
			}
		}
	}()
	go func() { errorsCh <- server.ListenAndServe() }()
	log.Printf("conversion-api listening on %s; apps=%d; x_ready=%t", *listen, len(config.Apps), config.Ready())
	var result error
	select {
	case <-ctx.Done():
	case err := <-errorsCh:
		if !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && result == nil {
		result = err
	}
	<-workerDone
	return result
}
