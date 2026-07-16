package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	fakeServerID = "cutoverfake-server"
	fakeUserID   = "cutoverfake-user"
	fakeToken    = "cutoverfake-token"
)

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /System/Info/Public", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"Id":         fakeServerID,
			"ServerId":   fakeServerID,
			"ServerName": "Cutover Fake Emby",
			"Version":    "4.9.0.0",
		})
	})
	mux.HandleFunc("POST /Users/AuthenticateByName", authenticate)
	mux.HandleFunc("POST /Sessions/Logout", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func authenticate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var request struct {
		Username string `json:"Username"`
		Password string `json:"Pw"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.Username) == "" || strings.TrimSpace(request.Password) == "" {
		http.Error(w, "invalid authentication request", http.StatusBadRequest)
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid authentication request", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"AccessToken": fakeToken,
		"ServerId":    fakeServerID,
		"ServerName":  "Cutover Fake Emby",
		"Version":     "4.9.0.0",
		"User":        map[string]string{"Id": fakeUserID},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeReadyFile(path, address string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("--ready-file is required")
	}
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".cutoverfake-ready-")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.WriteString("http://" + address + "\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func run(args []string) error {
	flags := flag.NewFlagSet("cutoverfake", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	readyFile := flags.String("ready-file", "", "file to atomically receive the fake base URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := writeReadyFile(*readyFile, listener.Addr().String()); err != nil {
		return err
	}

	server := &http.Server{Handler: handler(), ReadHeaderTimeout: 5 * time.Second}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	go func() {
		<-signals
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
