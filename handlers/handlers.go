package handlers

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/electr1fy0/harbor/service"
)

type DeployRequest struct {
	Name   string  `json:"name"`
	Image  string  `json:"image"`
	Memory int64   `json:"memory"`
	CPU    float64 `json:"cpu"`
}

type GitBuildRequest struct {
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
	Name    string `json:"name"`
	Builder string `json:"builder"`
}

type DeployFromGitRequest struct {
	RepoURL string  `json:"repo_url"`
	Branch  string  `json:"branch"`
	Name    string  `json:"name"`
	Builder string  `json:"builder"`
	Memory  int64   `json:"memory"`
	CPU     float64 `json:"cpu"`
}

type Handler struct {
	Service *service.Service
}

func (h *Handler) WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (h *Handler) CreateDeployment(w http.ResponseWriter, r *http.Request) {
	var req DeployRequest
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

	if err := h.Service.PullImage(r.Context(), req.Image); err != nil {
		http.Error(w, "failed to pull image: "+err.Error(), http.StatusInternalServerError)
		return
	}

	deployment, err := h.Service.Deploy(r.Context(), service.DeployOpts{
		Name:   req.Name,
		Image:  req.Image,
		Memory: req.Memory,
		CPU:    req.CPU,
	})
	if err != nil {
		http.Error(w, "failed to deploy: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.WriteJSON(w, http.StatusOK, deployment)
}

func (h *Handler) ListDeployments(w http.ResponseWriter, r *http.Request) {
	deployments, err := h.Service.List(r.Context())
	if err != nil {
		http.Error(w, "failed to list deployments: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.WriteJSON(w, http.StatusOK, deployments)
}

func (h *Handler) StopDeployment(w http.ResponseWriter, r *http.Request) {
	if err := h.Service.Stop(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "failed to stop container: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.WriteJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (h *Handler) StartDeployment(w http.ResponseWriter, r *http.Request) {
	if err := h.Service.Start(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "failed to start container: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.WriteJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (h *Handler) DeleteDeployment(w http.ResponseWriter, r *http.Request) {
	if err := h.Service.Delete(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "failed to remove container: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.WriteJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) GetDeploymentLogs(w http.ResponseWriter, r *http.Request) {
	result, err := h.Service.Logs(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "failed to get logs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer result.Close()

	w.Header().Set("Content-Type", "text/plain")
	io.Copy(w, result)
}

func (h *Handler) BuildFromGit(w http.ResponseWriter, r *http.Request) {
	var req GitBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.RepoURL == "" {
		http.Error(w, "repo_url is required", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	image, err := h.Service.BuildFromGit(r.Context(), service.BuildOpts{
		RepoURL: req.RepoURL,
		Branch:  req.Branch,
		Name:    req.Name,
		Builder: req.Builder,
	})
	if err != nil {
		http.Error(w, "build failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.WriteJSON(w, http.StatusOK, map[string]string{"image": image})
}

func (h *Handler) BuildAndDeploy(w http.ResponseWriter, r *http.Request) {
	var req DeployFromGitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.RepoURL == "" {
		http.Error(w, "repo_url is required", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	deployment, err := h.Service.BuildAndDeploy(r.Context(),
		service.BuildOpts{
			RepoURL: req.RepoURL,
			Branch:  req.Branch,
			Name:    req.Name,
			Builder: req.Builder,
		},
		service.DeployOpts{
			Name:   req.Name,
			Memory: req.Memory,
			CPU:    req.CPU,
		},
	)
	if err != nil {
		http.Error(w, "build and deploy failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.WriteJSON(w, http.StatusOK, deployment)
}
