package daemon

import "paas/internal/model"

// Request describes a local control-plane operation.
type Request struct {
	Name        string `json:"Name,omitempty"`
	SourceDir   string `json:"SourceDir,omitempty"`
	RepoURL     string `json:"RepoURL,omitempty"`
	Ref         string `json:"Ref,omitempty"`
	RootDir     string `json:"RootDir,omitempty"`
	ReleaseID   string `json:"ReleaseID,omitempty"`
	ServiceType string `json:"ServiceType,omitempty"`
	AppName     string `json:"AppName,omitempty"`
	ServiceName string `json:"ServiceName,omitempty"`
}

type AppView struct {
	Application model.Application
	Deployment  *model.Deployment
}

type Response struct {
	URL        string              `json:"URL,omitempty"`
	Deployment *model.Deployment   `json:"Deployment,omitempty"`
	Apps       []AppView           `json:"Apps,omitempty"`
	Releases   []model.ReleaseView `json:"Releases,omitempty"`
	Logs       string              `json:"Logs,omitempty"`
	Error      string              `json:"Error,omitempty"`
	Service    *model.Service      `json:"Service,omitempty"`
	Services   []model.Service     `json:"Services,omitempty"`
	Binding    *model.Binding      `json:"Binding,omitempty"`
}
