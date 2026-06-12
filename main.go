package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type deployRequest struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type deployResponse struct {
	ContainerID string                    `json:"container_id"`
	Status      string                    `json:"status"`
	Container   container.InspectResponse `json:"container"`
}

var harborNetwork = "harbor-network-1"

func main() {
	cli, err := client.New(client.WithHost("unix:///Users/ayush/.orbstack/run/docker.sock"))
	if err != nil {
		log.Fatalf("failed to create docker client: %v", err)
	}

	http.HandleFunc("POST /deployments", createDeployment(cli))
	http.HandleFunc("GET /deployments", listDeployments(cli))
	http.HandleFunc("POST /deployments/{id}/stop", stopDeployment(cli))
	http.HandleFunc("POST /deployments/{id}/start", startDeployment(cli))
	http.HandleFunc("DELETE /deployments/{id}", deleteDeployment(cli))
	http.HandleFunc("GET /deployments/{id}/logs", getDeploymentLogs(cli))

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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func createDeployment(cli *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req deployRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		if req.Image == "" {
			http.Error(w, "image is required", http.StatusBadRequest)
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
			Name: req.Name,
			Config: &container.Config{
				Env: []string{
					"POSTGRES_USER=harbordbuser",
					"POSTGRES_PASSWORD=harbordbpass",
				},
			},
			Image: req.Image,
		})
		if err != nil {
			http.Error(w, "failed to create container: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if _, err := cli.ContainerStart(ctx, result.ID, client.ContainerStartOptions{}); err != nil {
			http.Error(w, "failed to start container: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if _, err := cli.NetworkConnect(ctx, harborNetwork, client.NetworkConnectOptions{
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

		writeJSON(w, http.StatusOK, deployResponse{
			ContainerID: result.ID,
			Status:      "started",
			Container:   inspect.Container,
		})
	}
}

func listDeployments(cli *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := cli.ContainerList(r.Context(), client.ContainerListOptions{
			All:     true,
			Filters: client.Filters{}.Add("network", harborNetwork),
		})
		if err != nil {
			http.Error(w, "failed to list containers: "+err.Error(), http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, result.Items)
	}
}

func stopDeployment(cli *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := cli.ContainerStop(r.Context(), id, client.ContainerStopOptions{}); err != nil {
			http.Error(w, "failed to stop container: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
	}
}

func startDeployment(cli *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := cli.ContainerStart(r.Context(), id, client.ContainerStartOptions{}); err != nil {
			http.Error(w, "failed to start container: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
	}
}

func deleteDeployment(cli *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := cli.ContainerRemove(r.Context(), id, client.ContainerRemoveOptions{
			Force: true,
		}); err != nil {
			http.Error(w, "failed to remove container: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

func getDeploymentLogs(cli *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		result, err := cli.ContainerLogs(r.Context(), id, client.ContainerLogsOptions{
			ShowStdout: true,
			ShowStderr: true,
		})
		if err != nil {
			http.Error(w, "failed to get logs: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer result.Close()

		w.Header().Set("Content-Type", "text/plain")
		io.Copy(w, result)
	}
}
