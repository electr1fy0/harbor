package daemon

import "paas/internal/model"

// Request describes a local control-plane operation.
type Request struct {
	Name      string
	SourceDir string
}

type AppView struct {
	Application model.Application
	Deployment  *model.Deployment
}

type Response struct {
	URL        string
	Deployment *model.Deployment
	Apps       []AppView
	Logs       string
	Error      string
}
