package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const defaultNetwork = "3a4928457ca6aa6b5cbae418fe577db8b7e4324d9d328f05f8d8b87af8eecee1"

var hostMap = struct {
	sync.RWMutex
	mappings map[string]*httputil.ReverseProxy
}{mappings: make(map[string]*httputil.ReverseProxy)}

type deployRequest struct {
	Image string `json:"image"`
	Host  string `json:"host"`
}

type deployResponse struct {
	ContainerID string                    `json:"container_id"`
	Status      string                    `json:"status"`
	Host        string                    `json:"host"`
	Container   container.InspectResponse `json:"container"`
}

func main() {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		log.Fatalf("failed to create docker client: %v", err)
	}

	http.HandleFunc("POST /deploy", deployHandler(cli))
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

	hostMap.RLock()
	proxy, ok := hostMap.mappings[host]
	hostMap.RUnlock()

	if !ok {
		http.Error(w, "no backend for host", http.StatusBadGateway)
		return
	}

	proxy.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func deployHandler(cli *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req deployRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Image == "" {
			http.Error(w, "image is required", http.StatusBadRequest)
			return
		}
		if req.Host == "" {
			http.Error(w, "host is required", http.StatusBadRequest)
			return
		}

		ctx := r.Context()

		pullResp, err := cli.ImagePull(ctx, req.Image, client.ImagePullOptions{})
		if err != nil {
			http.Error(w, "failed to pull image: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer pullResp.Close()
		pullResp.Wait(ctx)

		result, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Image: req.Image,
			Config: &container.Config{
				Env: []string{
					"POSTGRES_USER=harbordbuser",
					"POSTGRES_PASSWORD=harbordbpass",
				},
			},
		})
		if err != nil {
			http.Error(w, "failed to create container: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if _, err := cli.ContainerStart(ctx, result.ID, client.ContainerStartOptions{}); err != nil {
			http.Error(w, "failed to start container: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if _, err := cli.NetworkConnect(ctx, defaultNetwork, client.NetworkConnectOptions{
			Container:      result.ID,
			EndpointConfig: &network.EndpointSettings{},
		}); err != nil {
			http.Error(w, "failed to connect to network: "+err.Error(), http.StatusInternalServerError)
			return
		}

		inspect, err := cli.ContainerInspect(ctx, result.ID, client.ContainerInspectOptions{})
		if err != nil {
			http.Error(w, "failed to inspect container: "+err.Error(), http.StatusInternalServerError)
			return
		}

		var containerIP string
		if netSettings, ok := inspect.Container.NetworkSettings.Networks[defaultNetwork]; ok {
			containerIP = netSettings.IPAddress.String()
		}
		if containerIP == "" {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not determine container ip"})
			return
		}

		target, _ := url.Parse("http://" + containerIP)
		proxy := httputil.NewSingleHostReverseProxy(target)

		hostMap.Lock()
		hostMap.mappings[req.Host] = proxy
		hostMap.Unlock()

		writeJSON(w, http.StatusOK, deployResponse{
			ContainerID: result.ID,
			Status:      "started",
			Host:        req.Host,
			Container:   inspect.Container,
		})
	}
}
