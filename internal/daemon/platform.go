package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"

	"paas/internal/deployment"
	"paas/internal/store"
)

func NewPlatformServer(socketPath string, service *deployment.Service, routerAddress ...string) *Server {
	port := "8080"
	if len(routerAddress) > 0 {
		if _, p, err := net.SplitHostPort(routerAddress[0]); err == nil {
			port = p
		}
	}
	server := NewServer(socketPath)
	ping := server.http.Handler
	mux := http.NewServeMux()
	mux.Handle("/ping", ping)
	for _, command := range []string{"deploy", "apps", "inspect", "logs", "stop"} {
		command := command
		mux.HandleFunc("/"+command, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				w.WriteHeader(405)
				_ = json.NewEncoder(w).Encode(Response{Error: "method not allowed"})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			var input *Request
			if err := decoder.Decode(&input); err != nil || input == nil {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(Response{Error: "invalid JSON request"})
				return
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(Response{Error: "request must contain one JSON object"})
				return
			}
			var output Response
			var err error
			switch command {
			case "deploy":
				output.Deployment, err = service.Deploy(r.Context(), input.SourceDir, input.Name)
			case "inspect":
				output.Deployment, err = service.Inspect(input.Name)
			case "stop":
				output.Deployment, err = service.Stop(r.Context(), input.Name)
			case "logs":
				output.Logs, err = service.Logs(input.Name)
			case "apps":
				apps, listErr := service.Apps()
				err = listErr
				output.Apps = make([]AppView, 0, len(apps))
				for _, app := range apps {
					d, getErr := service.Inspect(app.Name)
					if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
						err = getErr
						break
					}
					output.Apps = append(output.Apps, AppView{Application: app, Deployment: d})
				}
			}
			if err != nil {
				output.Error = err.Error()
				status := 400
				if errors.Is(err, store.ErrNotFound) {
					status = 404
				}
				w.WriteHeader(status)
			}
			if output.Deployment != nil {
				output.URL = "http://" + output.Deployment.AppName + ".localhost:" + port
			}
			_ = json.NewEncoder(w).Encode(output)
		})
	}
	server.http.Handler = mux
	return server
}
