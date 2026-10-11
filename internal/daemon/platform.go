package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"

	capacitymetrics "paas/internal/capacity"
	"paas/internal/config"
	"paas/internal/deployment"
	"paas/internal/model"
	"paas/internal/onboarding"
	"paas/internal/source"
	"paas/internal/store"
)

type ServiceManager interface {
	AddService(ctx context.Context, serviceType, name string) (*model.Service, error)
	Link(ctx context.Context, appName, serviceName string) (*model.Binding, error)
	List(ctx context.Context) ([]model.Service, error)
}

func NewPlatformServer(socketPath string, service *deployment.Service, routerAddress ...string) *Server {
	return NewPlatformServerWithServicesAndBuildUser(socketPath, service, nil, "paas-build", routerAddress...)
}

func NewPlatformServerWithServices(socketPath string, service *deployment.Service, services ServiceManager, routerAddress ...string) *Server {
	return NewPlatformServerWithServicesAndBuildUser(socketPath, service, services, "paas-build", routerAddress...)
}

func NewPlatformServerWithServicesAndBuildUser(socketPath string, service *deployment.Service, services ServiceManager, buildUser string, routerAddress ...string) *Server {
	port := "8080"
	if len(routerAddress) > 0 {
		if _, p, err := net.SplitHostPort(routerAddress[0]); err == nil {
			port = p
		}
	}
	server := NewServer(socketPath)
	dataDir := ""
	if service != nil {
		dataDir = service.CapacityDataDir()
	}
	capacityCollector := capacitymetrics.New(dataDir)
	server.onboardingJobs = onboarding.NewManager(service, services, buildUser)
	ping := server.http.Handler
	mux := http.NewServeMux()
	mux.Handle("/ping", ping)
	for _, command := range []string{"deploy", "apps", "inspect", "logs", "stop", "releases", "rollback", "add", "link", "services", "settings", "configure", "capacity"} {
		command := command
		mux.HandleFunc("/"+command, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				w.WriteHeader(405)
				writeResponse(w, command, Response{Error: "method not allowed"})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			var input *Request
			if err := decoder.Decode(&input); err != nil || input == nil {
				w.WriteHeader(400)
				writeResponse(w, command, Response{Error: "invalid JSON request"})
				return
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				w.WriteHeader(400)
				writeResponse(w, command, Response{Error: "request must contain one JSON object"})
				return
			}
			var output Response
			var err error
			switch command {
			case "deploy":
				if err = validateDeployRequest(input); err == nil && service != nil {
					sourceDir := input.SourceDir
					cleanup := func() {}
					if input.RepoURL != "" {
						sourceDir, cleanup, err = source.CheckoutGitHubAs(r.Context(), input.RepoURL, input.Ref, input.RootDir, buildUser)
					}
					if err == nil {
						defer cleanup()
						output.Deployment, err = service.Deploy(r.Context(), sourceDir, input.Name)
					}
				}
			case "inspect":
				if service != nil {
					output.Deployment, err = service.Inspect(input.Name)
				}
			case "stop":
				if service != nil {
					output.Deployment, err = service.Stop(r.Context(), input.Name)
				}
			case "logs":
				if service != nil {
					output.Logs, err = service.LogsForDeployment(input.Name, input.DeploymentID)
				}
			case "releases":
				if service != nil {
					output.Releases, err = service.Releases(input.Name)
				}
			case "rollback":
				if service != nil {
					output.Deployment, err = service.Rollback(r.Context(), input.Name, input.ReleaseID)
				}
			case "apps":
				if service != nil {
					apps, listErr := service.Apps()
					err = listErr
					output.Apps = make([]AppView, 0, len(apps))
					for _, app := range apps {
						legacy, inspectErr := service.Inspect(app.Name)
						if inspectErr != nil && !errors.Is(inspectErr, store.ErrNotFound) {
							err = inspectErr
							break
						}
						production, productionErr := service.Production(app.Name)
						if productionErr != nil {
							err = productionErr
							break
						}
						latestAttempt, latestErr := service.LatestAttempt(app.Name)
						if latestErr != nil {
							err = latestErr
							break
						}
						output.Apps = append(output.Apps, AppView{
							Application: app, Deployment: legacy,
							Production: production, LatestAttempt: latestAttempt,
						})
					}
				}
			case "add":
				if services != nil {
					output.Service, err = services.AddService(r.Context(), input.ServiceType, input.Name)
				} else {
					err = errors.New("services not supported by this daemon")
				}
			case "link":
				if services != nil {
					output.Binding, err = services.Link(r.Context(), input.AppName, input.ServiceName)
				} else {
					err = errors.New("services not supported by this daemon")
				}
			case "services":
				if services != nil {
					output.Services, err = services.List(r.Context())
				} else {
					err = errors.New("services not supported by this daemon")
				}
			case "settings":
				if service != nil {
					output.Settings, err = service.Settings(input.Name)
				} else {
					err = errors.New("application settings are unavailable")
				}
			case "configure":
				if service != nil {
					output.Settings, err = service.Configure(r.Context(), input.Name, deployment.ConfigureOptions{
						Environment: input.Environment, RemoveEnvironment: input.RemoveEnvironment,
						Domains: input.Domains, MemoryLimit: input.MemoryLimit,
						CPUQuota: input.CPUQuota, TasksLimit: input.TasksLimit,
					})
				} else {
					err = errors.New("application settings are unavailable")
				}
			case "capacity":
				if input.Name != "" && !config.ValidName(input.Name) {
					err = errors.New("invalid application name")
					break
				}
				var runtimes []capacitymetrics.RuntimeLimit
				if service != nil {
					var configured []deployment.CapacityRuntime
					configured, err = service.CapacityRuntimes(r.Context(), input.Name)
					for _, runtime := range configured {
						memory, _ := config.ParseMemoryBytes(runtime.MemoryLimit)
						cpuQuota := runtime.CPUQuota
						if cpuQuota <= 0 && runtime.ID != "" {
							cpuQuota = config.DefaultCPU
						}
						tasks := runtime.TasksLimit
						if tasks <= 0 && runtime.ID != "" {
							tasks = config.DefaultTasks
						}
						runtimes = append(runtimes, capacitymetrics.RuntimeLimit{
							Identity: runtime.ID, Name: runtime.AppName, State: string(runtime.State), PID: runtime.PID,
							ControlGroup: runtime.ControlGroup, MemoryLimit: memory,
							CPUQuotaCores: cpuQuota, TasksLimit: tasks,
						})
					}
				} else if input.Name != "" {
					err = store.ErrNotFound
				}
				if err == nil {
					var snapshot capacitymetrics.Snapshot
					snapshot, err = capacityCollector.Sample(r.Context(), runtimes)
					if err == nil {
						output.Capacity = &snapshot
					}
				}
			}
			if err != nil {
				output.Error = err.Error()
				status := 400
				if errors.Is(err, store.ErrNotFound) {
					status = 404
				} else if errors.Is(err, store.ErrDomainInUse) {
					status = 409
				}
				w.WriteHeader(status)
			}
			if output.Deployment != nil {
				output.URL = "http://" + output.Deployment.AppName + ".localhost:" + port
				if service != nil {
					if publicURL := service.PublicURL(output.Deployment.AppName); publicURL != "" {
						output.URL = publicURL
					}
				}
			}
			writeResponse(w, command, output)
		})
	}
	registerOnboardingRoutes(mux, server)
	registerDiagnosticsRoute(mux, service, server.onboardingJobs.CanRedeploy)
	server.http.Handler = mux
	return server
}

func validateDeployRequest(input *Request) error {
	if input.RepoURL == "" {
		if input.Ref != "" || input.RootDir != "" {
			return errors.New("Ref and RootDir require RepoURL")
		}
		return nil
	}
	if input.SourceDir != "" {
		return errors.New("SourceDir and RepoURL cannot be used together")
	}
	return nil
}

func writeResponse(w http.ResponseWriter, command string, response Response) {
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("write %s response: %v", command, err)
	}
}
