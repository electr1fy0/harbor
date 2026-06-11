package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type deployRequest struct {
	Image string `json:"image"`
}

type deployResponse struct {
	ContainerID string                    `json:"container_id"`
	Status      string                    `json:"status"`
	Container   container.InspectResponse `json:"container"`
}

func main() {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		log.Fatalf("failed to create docker client: %v", err)
	}

	http.HandleFunc("POST /deploy", deployHandler(cli))

	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
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
