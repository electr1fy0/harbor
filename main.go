package main

import (
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/electr1fy0/harbor/handlers"
	"github.com/electr1fy0/harbor/service"
	"github.com/moby/moby/client"
)

func main() {
	cli, err := client.New(client.WithHost("unix:///Users/ayush/.orbstack/run/docker.sock"))
	if err != nil {
		log.Fatalf("failed to create docker client: %v", err)
	}

	svc := &service.Service{Client: cli}
	h := &handlers.Handler{Service: svc}

	http.HandleFunc("POST /deployments", h.CreateDeployment)
	http.HandleFunc("GET /deployments", h.ListDeployments)
	http.HandleFunc("POST /deployments/{id}/stop", h.StopDeployment)
	http.HandleFunc("POST /deployments/{id}/start", h.StartDeployment)
	http.HandleFunc("DELETE /deployments/{id}", h.DeleteDeployment)
	http.HandleFunc("GET /deployments/{id}/logs", h.GetDeploymentLogs)
	http.HandleFunc("POST /builds", h.BuildFromGit)
	http.HandleFunc("POST /deployments/build", h.BuildAndDeploy)

	go func() {
		log.Println("api listening on :8080")
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()

	http.HandleFunc("/", proxyHandler)

	log.Println("proxy listening on :80")
	log.Fatal(http.ListenAndServe(":80", nil))
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}

	name := strings.Split(host, ".")[0]
	if name == "" {
		http.Error(w, "no container for host", http.StatusBadGateway)
		return
	}

	target, _ := url.Parse("http://" + name)
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
}
