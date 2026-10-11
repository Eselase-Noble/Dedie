// Command dedie-desktop runs the same engine and interface as the server for a
// single person on their own machine. It listens on a random loopback port,
// which nothing outside the machine can reach, and opens the interface in an
// application window of an installed Chromium-family browser, falling back to
// the default browser.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"dedie/apps/internal/httpapi"
	"dedie/engine/app"
	"dedie/web"
)

func main() {
	dataDir := flag.String("data", app.DefaultDataDir(), "directory for the metadata database, master key and backups")
	noOpen := flag.Bool("no-open", false, "do not open a window; print the address instead")
	flag.Parse()

	a, err := app.Open(*dataDir)
	if err != nil {
		log.Fatalf("open data directory %s: %v", *dataDir, err)
	}
	defer a.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	url := fmt.Sprintf("http://%s/", ln.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); a.RunWorker(ctx) }()

	srv := &http.Server{Handler: httpapi.New(a, httpapi.Options{UI: web.UI()}), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)

	fmt.Println("dedie is running at", url)
	fmt.Println("Press Ctrl+C to quit. Running migrations pause and resume on the next start.")
	if !*noOpen {
		openWindow(url)
	}

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	select {
	case <-workerDone:
	case <-time.After(30 * time.Second):
	}
	os.Exit(0)
}

// openWindow prefers a chromeless application window and falls back to a
// regular browser tab.
func openWindow(url string) {
	for _, browser := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"} {
		if path, err := exec.LookPath(browser); err == nil {
			if exec.Command(path, "--app="+url, "--new-window").Start() == nil {
				return
			}
		}
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("Open this address in your browser:", url)
	}
}
