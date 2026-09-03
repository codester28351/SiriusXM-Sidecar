package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	sidecar "github.com/codester28351/siriusxm-sidecar"
)

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
func detectLANIP() string {
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	return "127.0.0.1"
}
func main() {
	if len(os.Args) != 3 {
		log.Fatalf("usage: %s USERNAME PASSWORD", os.Args[0])
	}
	host := envOr("SIDECAR_HOST", "0.0.0.0")
	port := envOr("SIDECAR_PORT", "8091")
	lan := envOr("SIDECAR_LAN_IP", detectLANIP())
	base := envOr("SIDECAR_BASE_URL", "http://"+lan+":"+port)
	if err := sidecar.ValidateBaseURL(base); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	p := sidecar.NewSiriusProvider(os.Args[1], os.Args[2])
	if err := p.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer p.Stop()
	srv := &http.Server{Addr: host + ":" + port, Handler: sidecar.NewAPI(p, base).Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Printf("SiriusXM sidecar listening on %s", srv.Addr)
	log.Printf("LAN base URL: %s", base)
	log.Printf("Station list: %s/api/stations", base)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
