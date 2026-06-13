package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	packclient "github.com/buildpacks/pack/pkg/client"
	packimage "github.com/buildpacks/pack/pkg/image"
	"github.com/buildpacks/pack/pkg/logging"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

var HarborNetwork = "harbor-network-1"

type DeployOpts struct {
	Name   string
	Image  string
	Memory int64
	CPU    float64
}

type Deployment struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status string `json:"status"`
	State  string `json:"state"`
}

type BuildOpts struct {
	RepoURL string
	Branch  string
	Name    string
	Builder string
}

type Service struct {
	Client *client.Client
}

func toDeployment(c container.Summary) Deployment {
	name := c.Names[0]
	if len(name) > 0 && name[0] == '/' {
		name = name[1:]
	}
	return Deployment{
		ID:     c.ID,
		Name:   name,
		Image:  c.Image,
		Status: c.Status,
		State:  string(c.State),
	}
}

func toDeploymentFromInspect(c container.InspectResponse) Deployment {
	return Deployment{
		ID:     c.ID,
		Name:   c.Name,
		Image:  c.Config.Image,
		Status: inspectStatus(c.State),
		State:  string(c.State.Status),
	}
}

func inspectStatus(s *container.State) string {
	switch s.Status {
	case container.StateRunning:
		return "Running"
	case container.StatePaused:
		return "Paused"
	case container.StateExited:
		return fmt.Sprintf("Exited (%d)", s.ExitCode)
	default:
		return string(s.Status)
	}
}

func (s *Service) PullImage(ctx context.Context, image string) error {
	pullResp, err := s.Client.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer pullResp.Close()
	return pullResp.Wait(ctx)
}

func (s *Service) Deploy(ctx context.Context, opts DeployOpts) (Deployment, error) {
	result, err := s.Client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: opts.Name,
		Config: &container.Config{
			Env: []string{
				"POSTGRES_USER=harbordbuser",
				"POSTGRES_PASSWORD=harbordbpass",
			},
		},
		HostConfig: &container.HostConfig{
			Resources: container.Resources{
				Memory:   opts.Memory * 1024 * 1024,
				NanoCPUs: int64(opts.CPU * 1e9),
			},
		},
		Image: opts.Image,
	})
	if err != nil {
		return Deployment{}, err
	}

	if _, err := s.Client.ContainerStart(ctx, result.ID, client.ContainerStartOptions{}); err != nil {
		return Deployment{}, err
	}

	if _, err := s.Client.NetworkConnect(ctx, HarborNetwork, client.NetworkConnectOptions{
		Container:      result.ID,
		EndpointConfig: &network.EndpointSettings{},
	}); err != nil {
		return Deployment{}, err
	}

	inspect, err := s.Client.ContainerInspect(ctx, result.ID, client.ContainerInspectOptions{})
	if err != nil {
		return Deployment{}, err
	}

	return toDeploymentFromInspect(inspect.Container), nil
}

func (s *Service) List(ctx context.Context) ([]Deployment, error) {
	result, err := s.Client.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("network", HarborNetwork),
	})
	if err != nil {
		return nil, err
	}

	deployments := make([]Deployment, len(result.Items))
	for i, c := range result.Items {
		deployments[i] = toDeployment(c)
	}
	return deployments, nil
}

func (s *Service) Stop(ctx context.Context, id string) error {
	_, err := s.Client.ContainerStop(ctx, id, client.ContainerStopOptions{})
	return err
}

func (s *Service) Start(ctx context.Context, id string) error {
	_, err := s.Client.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (s *Service) Delete(ctx context.Context, id string) error {
	_, err := s.Client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{
		Force: true,
	})
	return err
}

func (s *Service) Logs(ctx context.Context, id string) (io.ReadCloser, error) {
	return s.Client.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
}

func (s *Service) BuildFromGit(ctx context.Context, opts BuildOpts) (string, error) {
	srcDir, err := os.MkdirTemp("", "harbor-build-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(srcDir)

	if err := fetchSource(ctx, opts.RepoURL, opts.Branch, srcDir); err != nil {
		return "", err
	}

	imageName := filepath.Base(opts.Name)
	if imageName == "." || imageName == "" {
		imageName = "harbor-build"
	}
	imageRef := fmt.Sprintf("%s:latest", imageName)

	builder := opts.Builder
	if builder == "" {
		builder = os.Getenv("HARBOR_BUILDER")
	}
	if builder == "" {
		builder = "gcr.io/buildpacks/builder:v1"
	}

	var logBuf bytes.Buffer
	logger := logging.NewSimpleLogger(&logBuf)

	packClient, err := packclient.NewClient(
		packclient.WithLogger(logger),
		packclient.WithDockerClient(s.Client),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create pack client: %w", err)
	}

	err = packClient.Build(ctx, packclient.BuildOptions{
		Image:        imageRef,
		AppPath:      srcDir,
		Builder:      builder,
		PullPolicy:   packimage.PullIfNotPresent,
		TrustBuilder: func(string) bool { return true },
	})
	if err != nil {
		return "", fmt.Errorf("pack build failed: %w\n%s", err, logBuf.String())
	}

	return imageRef, nil
}

func fetchSource(ctx context.Context, repoURL, ref, dst string) error {
	u, err := url.Parse(repoURL)
	if err != nil {
		return fmt.Errorf("invalid repo URL: %w", err)
	}

	switch u.Host {
	case "github.com":
		return fetchGitHubTarball(ctx, u, ref, dst)
	default:
		return fmt.Errorf("unsupported repository host: %s (only github.com is supported)", u.Host)
	}
}

func fetchGitHubTarball(ctx context.Context, u *url.URL, ref, dst string) error {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return fmt.Errorf("invalid GitHub URL: %s", u.String())
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	if ref == "" {
		ref = "HEAD"
	}

	archiveURL := fmt.Sprintf("https://github.com/%s/%s/archive/%s.tar.gz", owner, repo, url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", archiveURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", archiveURL, resp.Status)
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}

		path := strings.SplitN(filepath.Clean(header.Name), string(filepath.Separator), 2)
		if len(path) < 2 {
			continue
		}
		target := filepath.Join(dst, path[1])

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}

	return nil
}

func (s *Service) BuildAndDeploy(ctx context.Context, buildOpts BuildOpts, deployOpts DeployOpts) (Deployment, error) {
	image, err := s.BuildFromGit(ctx, buildOpts)
	if err != nil {
		return Deployment{}, err
	}
	deployOpts.Image = image
	return s.Deploy(ctx, deployOpts)
}
