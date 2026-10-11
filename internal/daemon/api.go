package daemon

import (
	"paas/internal/capacity"
	"paas/internal/model"
)

// Request describes a local control-plane operation.
type Request struct {
	Name              string            `json:"Name,omitempty"`
	DeploymentID      string            `json:"DeploymentID,omitempty"`
	SourceDir         string            `json:"SourceDir,omitempty"`
	RepoURL           string            `json:"RepoURL,omitempty"`
	Ref               string            `json:"Ref,omitempty"`
	RootDir           string            `json:"RootDir,omitempty"`
	ReleaseID         string            `json:"ReleaseID,omitempty"`
	ServiceType       string            `json:"ServiceType,omitempty"`
	AppName           string            `json:"AppName,omitempty"`
	ServiceName       string            `json:"ServiceName,omitempty"`
	Environment       map[string]string `json:"Environment,omitempty"`
	RemoveEnvironment []string          `json:"RemoveEnvironment,omitempty"`
	Domains           *[]string         `json:"Domains,omitempty"`
	MemoryLimit       *string           `json:"MemoryLimit,omitempty"`
	CPUQuota          *float64          `json:"CPUQuota,omitempty"`
	TasksLimit        *int              `json:"TasksLimit,omitempty"`
}

type AppView struct {
	Application   model.Application
	Deployment    *model.Deployment
	Production    *model.Deployment
	LatestAttempt *model.Deployment
}

type Response struct {
	URL         string              `json:"URL,omitempty"`
	Deployment  *model.Deployment   `json:"Deployment,omitempty"`
	Apps        []AppView           `json:"Apps,omitempty"`
	Releases    []model.ReleaseView `json:"Releases,omitempty"`
	Logs        string              `json:"Logs,omitempty"`
	Error       string              `json:"Error,omitempty"`
	Service     *model.Service      `json:"Service,omitempty"`
	Services    []model.Service     `json:"Services,omitempty"`
	Binding     *model.Binding      `json:"Binding,omitempty"`
	Settings    *model.Settings     `json:"settings,omitempty"`
	Capacity    *capacity.Snapshot  `json:"Capacity,omitempty"`
	Diagnostics *model.Diagnostics  `json:"diagnostics,omitempty"`
}
