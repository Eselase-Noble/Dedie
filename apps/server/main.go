// Command dedie-server runs the web interface, the HTTP API and the background
// worker that executes migrations.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dedie/apps/internal/httpapi"
	"dedie/engine/app"
	"dedie/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	dataDir := flag.String("data", app.DefaultDataDir(), "directory for the metadata database, master key and backups")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (PEM)")
	tlsKey := flag.String("tls-key", "", "TLS private key file (PEM)")
	behindProxy := flag.Bool("behind-tls-proxy", false, "a reverse proxy terminates TLS in front of this server; trust X-Forwarded-For and mark cookies Secure")
	noWorker := flag.Bool("no-worker", false, "serve the API only; do not execute migration jobs or scheduled backups in this process")
	flag.Parse()

	useTLS := *tlsCert != "" && *tlsKey != ""
	if !useTLS && !*behindProxy && !isLoopback(*addr) {
		log.Fatalf("refusing to serve plain HTTP on %s: sessions and database credentials would cross the network unencrypted.\n"+
			"Pass --tls-cert and --tls-key, or --behind-tls-proxy if a proxy terminates TLS.", *addr)
	}

	a, err := app.Open(*dataDir)
	if err != nil {
		log.Fatalf("open data directory %s: %v", *dataDir, err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		if !*noWorker {
			go a.RunScheduler(ctx)
			a.RunWorker(ctx)
		}
	}()

	// Listen before announcing the address, so that port 0 ("any free
	// port") can be reported as the port actually bound.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen on %s: %v", *addr, err)
	}
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	url := scheme + "://" + ln.Addr().String() + "/"

	srv := &http.Server{
		Handler:           httpapi.New(a, httpapi.Options{Secure: useTLS || *behindProxy, TrustProxy: *behindProxy, UI: web.UI()}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		log.Printf("dedie-server listening on %s (data: %s)", url, *dataDir)
		// A stable line on standard output for programs that start the
		// server and need its address, such as the desktop application.
		fmt.Println("DEDIE_URL=" + url)
		var err error
		if useTLS {
			err = srv.ServeTLS(ln, *tlsCert, *tlsKey)
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Print("shutting down; running migrations are handed back to the queue")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	select {
	case <-workerDone:
	case <-time.After(30 * time.Second):
		log.Print("worker did not stop in time")
	}
	os.Exit(0)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
